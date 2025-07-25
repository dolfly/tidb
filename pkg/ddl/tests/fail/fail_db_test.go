// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/ddl/schematracker"
	"github.com/pingcap/tidb/pkg/ddl/schemaver"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/store/mockstore"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testfailpoint"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/testutils"
	"go.opencensus.io/stats/view"
)

type failedSuite struct {
	cluster testutils.Cluster
	store   kv.Storage
	dom     *domain.Domain
}

func createFailDBSuite(t *testing.T) (s *failedSuite) {
	return createFailDBSuiteWithLease(t, 200*time.Millisecond)
}

func createFailDBSuiteWithLease(t *testing.T, lease time.Duration) (s *failedSuite) {
	s = new(failedSuite)
	var err error
	s.store, err = mockstore.NewMockStore(
		mockstore.WithClusterInspector(func(c testutils.Cluster) {
			mockstore.BootstrapWithSingleStore(c)
			s.cluster = c
		}),
	)
	require.NoError(t, err)
	session.SetSchemaLease(lease)
	s.dom, err = session.BootstrapSession(s.store)
	require.NoError(t, err)

	t.Cleanup(func() {
		s.dom.Close()
		require.NoError(t, s.store.Close())
		view.Stop()
	})

	return
}

// TestHalfwayCancelOperations tests the case that the schema is correct after the execution of operations are cancelled halfway.
func TestHalfwayCancelOperations(t *testing.T) {
	s := createFailDBSuite(t)
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/truncateTableErr", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/truncateTableErr"))
	}()
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("create database cancel_job_db")
	tk.MustExec("use cancel_job_db")

	// test for truncating table
	tk.MustExec("create table t(a int)")
	tk.MustExec("insert into t values(1)")
	_, err := tk.Exec("truncate table t")
	require.Error(t, err)

	// Make sure that the table's data has not been deleted.
	tk.MustQuery("select * from t").Check(testkit.Rows("1"))
	// Execute ddl statement reload schema
	tk.MustExec("alter table t comment 'test1'")

	tk = testkit.NewTestKit(t, s.store)
	tk.MustExec("use cancel_job_db")
	// Test schema is correct.
	tk.MustExec("select * from t")
	// test for renaming table
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/renameTableErr", `return("ty")`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/renameTableErr"))
	}()
	tk.MustExec("create table tx(a int)")
	tk.MustExec("insert into tx values(1)")
	err = tk.ExecToErr("rename table tx to ty")
	require.Error(t, err)
	tk.MustExec("create table ty(a int)")
	tk.MustExec("insert into ty values(2)")
	err = tk.ExecToErr("rename table ty to tz, tx to ty")
	require.Error(t, err)
	err = tk.ExecToErr("select * from tz")
	require.Error(t, err)
	err = tk.ExecToErr("rename table tx to ty, ty to tz")
	require.Error(t, err)
	tk.MustQuery("select * from ty").Check(testkit.Rows("2"))
	// Make sure that the table's data has not been deleted.
	tk.MustQuery("select * from tx").Check(testkit.Rows("1"))
	// Execute ddl statement reload schema.
	tk.MustExec("alter table tx comment 'tx'")

	tk = testkit.NewTestKit(t, s.store)
	tk.MustExec("use cancel_job_db")
	tk.MustExec("select * from tx")
	// test for exchanging partition
	limit := vardef.GetDDLErrorCountLimit()
	vardef.SetDDLErrorCountLimit(3)
	defer func() {
		vardef.SetDDLErrorCountLimit(limit)
	}()
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/exchangePartitionErr", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/exchangePartitionErr"))
	}()
	tk.MustExec("create table pt(a int) partition by hash (a) partitions 2")
	tk.MustExec("insert into pt values(1), (3), (5)")
	tk.MustExec("create table nt(a int)")
	tk.MustExec("insert into nt values(7)")
	err = tk.ExecToErr("alter table pt exchange partition p1 with table nt")
	require.Error(t, err)

	tk.MustQuery("select * from pt").Check(testkit.Rows("1", "3", "5"))
	tk.MustQuery("select * from nt").Check(testkit.Rows("7"))
	// Execute ddl statement reload schema.
	tk.MustExec("alter table pt comment 'pt'")

	tk = testkit.NewTestKit(t, s.store)
	tk.MustExec("use cancel_job_db")
	// Test schema is correct.
	tk.MustExec("select * from pt")

	// clean up
	tk.MustExec("drop database cancel_job_db")
}

// TestInitializeOffsetAndState tests the case that the column's offset and state don't be initialized in the file of executor.go when
// doing the operation of 'modify column'.
func TestInitializeOffsetAndState(t *testing.T) {
	s := createFailDBSuite(t)
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table t(a int, b int, c int)")
	defer tk.MustExec("drop table t")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/uninitializedOffsetAndState", `return(true)`))
	tk.MustExec("ALTER TABLE t MODIFY COLUMN b int FIRST;")
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/uninitializedOffsetAndState"))
}

func TestUpdateHandleFailed(t *testing.T) {
	s := createFailDBSuite(t)
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/errorUpdateReorgHandle", `1*return`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/errorUpdateReorgHandle"))
	}()
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("create database if not exists test_handle_failed")
	defer tk.MustExec("drop database test_handle_failed")
	tk.MustExec("use test_handle_failed")
	tk.MustExec("create table t(a int primary key, b int)")
	tk.MustExec("insert into t values(-1, 1)")
	tk.MustExec("alter table t add index idx_b(b)")
	result := tk.MustQuery("select count(*) from t use index(idx_b)")
	result.Check(testkit.Rows("1"))
	tk.MustExec("admin check index t idx_b")
}

func TestAddIndexFailed(t *testing.T) {
	s := createFailDBSuite(t)
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/mockBackfillRunErr", `1*return`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/mockBackfillRunErr"))
	}()
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("create database if not exists test_add_index_failed")
	defer tk.MustExec("drop database test_add_index_failed")
	tk.MustExec("use test_add_index_failed")

	tk.MustExec("create table t(a bigint PRIMARY KEY, b int)")
	for i := range 1000 {
		tk.MustExec(fmt.Sprintf("insert into t values(%v, %v)", i, i))
	}

	// Get table ID for split.
	dom := domain.GetDomain(tk.Session())
	is := dom.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test_add_index_failed"), ast.NewCIStr("t"))
	require.NoError(t, err)
	tblID := tbl.Meta().ID

	// Split the table.
	tableStart := tablecodec.GenTableRecordPrefix(tblID)
	s.cluster.SplitKeys(tableStart, tableStart.PrefixNext(), 100)

	tk.MustExec("alter table t add index idx_b(b)")
	tk.MustExec("admin check index t idx_b")
	tk.MustExec("admin check table t")
}

// TestFailSchemaSyncer test when the schema syncer is done,
// should prohibit DML executing until the syncer is restartd by loadSchemaInLoop.
func TestFailSchemaSyncer(t *testing.T) {
	s := createFailDBSuiteWithLease(t, 10*time.Second)
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t(a int)")
	defer tk.MustExec("drop table if exists t")
	originalRetryTimes := domain.SchemaOutOfDateRetryTimes.Load()
	domain.SchemaOutOfDateRetryTimes.Store(1)
	defer func() {
		domain.SchemaOutOfDateRetryTimes.Store(originalRetryTimes)
	}()
	require.True(t, s.dom.GetSchemaValidator().IsStarted())
	mockSyncer, ok := s.dom.DDL().SchemaSyncer().(*schemaver.MemSyncer)
	require.True(t, ok)

	// make reload failed.
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/infoschema/issyncer/ErrorMockReloadFailed", `return(true)`))
	mockSyncer.CloseSession()
	// wait the schemaValidator is stopped.
	for range 50 {
		if !s.dom.GetSchemaValidator().IsStarted() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.False(t, s.dom.GetSchemaValidator().IsStarted())
	_, err := tk.Exec("insert into t values(1)")
	require.Error(t, err)
	require.EqualError(t, err, "[domain:8027]Information schema is out of date: schema failed to update in 1 lease, please make sure TiDB can connect to TiKV")
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/infoschema/issyncer/ErrorMockReloadFailed"))
	// wait the schemaValidator is started.
	for range 50 {
		if s.dom.GetSchemaValidator().IsStarted() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, s.dom.GetSchemaValidator().IsStarted())
	err = tk.ExecToErr("insert into t values(1)")
	require.NoError(t, err)
}

func TestGenGlobalIDFail(t *testing.T) {
	s := createFailDBSuite(t)
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/mockGenGlobalIDFail"))
	}()
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("create database if not exists gen_global_id_fail")
	tk.MustExec("use gen_global_id_fail")

	sql1 := "create table t1(a bigint PRIMARY KEY, b int)"
	sql2 := `create table t2(a bigint PRIMARY KEY, b int) partition by range (a) (
			      partition p0 values less than (3440),
			      partition p1 values less than (61440),
			      partition p2 values less than (122880),
			      partition p3 values less than maxvalue)`
	sql3 := `truncate table t1`
	sql4 := `truncate table t2`

	testcases := []struct {
		sql     string
		table   string
		mockErr bool
	}{
		{sql1, "t1", true},
		{sql2, "t2", true},
		{sql1, "t1", false},
		{sql2, "t2", false},
		{sql3, "t1", true},
		{sql4, "t2", true},
		{sql3, "t1", false},
		{sql4, "t2", false},
	}

	for idx, test := range testcases {
		if test.mockErr {
			require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/mockGenGlobalIDFail", `return(true)`))
			_, err := tk.Exec(test.sql)
			require.Errorf(t, err, "the %dth test case '%s' fail", idx, test.sql)
		} else {
			require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/mockGenGlobalIDFail", `return(false)`))
			tk.MustExec(test.sql)
			tk.MustExec(fmt.Sprintf("insert into %s values (%d, 42)", test.table, rand.Intn(65536)))
			tk.MustExec(fmt.Sprintf("admin check table %s", test.table))
		}
	}
	tk.MustExec("admin check table t1")
	tk.MustExec("admin check table t2")
}

// TestRunDDLJobPanicEnableFastCreateTable tests recover panic with fast create table when run ddl job panic.
func TestRunDDLJobPanicEnableFastCreateTable(t *testing.T) {
	s := createFailDBSuite(t)
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("set global tidb_enable_fast_create_table=ON")
	testfailpoint.Enable(t, "github.com/pingcap/tidb/pkg/ddl/mockPanicInRunDDLJob", `1*panic("panic test")`)
	_, err := tk.Exec("create table t(c1 int, c2 int)")
	require.Error(t, err)
	require.EqualError(t, err, "[ddl:8214]Cancelled DDL job")
}

// TestRunDDLJobPanic tests recover panic when run ddl job panic.
func TestRunDDLJobPanic(t *testing.T) {
	s := createFailDBSuite(t)
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/mockPanicInRunDDLJob"))
	}()
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/mockPanicInRunDDLJob", `1*panic("panic test")`))
	_, err := tk.Exec("create table t(c1 int, c2 int)")
	require.Error(t, err)
	require.EqualError(t, err, "[ddl:8214]Cancelled DDL job")
}

func TestPartitionAddIndexGC(t *testing.T) {
	s := createFailDBSuite(t)
	tk := testkit.NewTestKit(t, s.store)
	if tk.MustQuery("select @@tidb_schema_cache_size > 0").Equal(testkit.Rows("1")) {
		// This test mock GC expire time exceeded, it's ok for infoschema v1 because it does not visit the network.
		// While in infoschema v2, SchemaTable call meta.ListTables and fail.
		t.Skip()
	}
	tk.MustExec("use test")
	tk.MustExec(`create table partition_add_idx (
	id int not null,
	hired date not null
	)
	partition by range( year(hired) ) (
	partition p1 values less than (1991),
	partition p5 values less than (2008),
	partition p7 values less than (2018)
	);`)
	tk.MustExec("insert into partition_add_idx values(1, '2010-01-01'), (2, '1990-01-01'), (3, '2001-01-01')")

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/mockUpdateCachedSafePoint", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/mockUpdateCachedSafePoint"))
	}()
	tk.MustExec("alter table partition_add_idx add index idx (id, hired)")
}

func TestModifyColumn(t *testing.T) {
	s := createFailDBSuite(t)
	tk := testkit.NewTestKit(t, schematracker.NewStorageDDLInjector(s.store))

	dom := domain.GetDomain(tk.Session())

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t;")

	tk.MustExec("create table t (a int not null default 1, b int default 2, c int not null default 0, primary key(c), index idx(b), index idx1(a), index idx2(b, c))")
	tk.MustExec("insert into t values(1, 2, 3), (11, 22, 33)")
	_, err := tk.Exec("alter table t change column c cc mediumint")
	require.EqualError(t, err, "[ddl:8200]Unsupported modify column: this column has primary key flag")
	tk.MustExec("alter table t change column b bb mediumint first")

	is := dom.InfoSchema()
	tbl, err := is.TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	cols := tbl.Meta().Columns
	colsStr := ""
	idxsStr := ""
	for _, col := range cols {
		colsStr += col.Name.L + " "
	}
	for _, idx := range tbl.Meta().Indices {
		idxsStr += idx.Name.L + " "
	}
	require.Len(t, cols, 3)
	require.Len(t, tbl.Meta().Indices, 3)
	tk.MustQuery("select * from t").Check(testkit.Rows("2 1 3", "22 11 33"))
	tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `bb` mediumint(9) DEFAULT NULL,\n" +
		"  `a` int(11) NOT NULL DEFAULT '1',\n" +
		"  `c` int(11) NOT NULL DEFAULT '0',\n" +
		"  PRIMARY KEY (`c`) /*T![clustered_index] CLUSTERED */,\n" +
		"  KEY `idx` (`bb`),\n" +
		"  KEY `idx1` (`a`),\n" +
		"  KEY `idx2` (`bb`,`c`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))
	tk.MustExec("admin check table t")
	tk.MustExec("insert into t values(111, 222, 333)")
	tk.MustGetErrMsg("alter table t change column a aa tinyint after c", "[types:1690]constant 222 overflows tinyint")
	tk.MustExec("alter table t change column a aa mediumint after c")
	tk.MustQuery("show create table t").Check(testkit.Rows("t CREATE TABLE `t` (\n" +
		"  `bb` mediumint(9) DEFAULT NULL,\n" +
		"  `c` int(11) NOT NULL DEFAULT '0',\n" +
		"  `aa` mediumint(9) DEFAULT NULL,\n" +
		"  PRIMARY KEY (`c`) /*T![clustered_index] CLUSTERED */,\n" +
		"  KEY `idx` (`bb`),\n" +
		"  KEY `idx1` (`aa`),\n" +
		"  KEY `idx2` (`bb`,`c`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"))
	tk.MustQuery("select * from t").Check(testkit.Rows("2 3 1", "22 33 11", "111 333 222"))
	tk.MustExec("admin check table t")

	// Test unsupported statements.
	tk.MustExec("create table t1(a int) partition by hash (a) partitions 2")
	tk.MustGetErrMsg("alter table t1 modify column a mediumint", "[ddl:8200]Unsupported modify column: table is partition table")
	tk.MustExec("create table t2(id int, a int, b int generated always as (abs(a)) virtual, c int generated always as (a+1) stored)")
	tk.MustGetErrMsg("alter table t2 modify column b mediumint", "[ddl:8200]Unsupported modify column: newCol IsGenerated false, oldCol IsGenerated true")
	tk.MustGetErrMsg("alter table t2 modify column c mediumint", "[ddl:8200]Unsupported modify column: newCol IsGenerated false, oldCol IsGenerated true")
	tk.MustGetErrMsg("alter table t2 modify column a mediumint generated always as(id+1) stored", "[ddl:8200]Unsupported modify column: newCol IsGenerated true, oldCol IsGenerated false")
	tk.MustGetErrMsg("alter table t2 modify column a mediumint", "[ddl:8200]Unsupported modify column: oldCol is a dependent column 'a' for generated column")

	// Test multiple rows of data.
	tk.MustExec("create table t3(a int not null default 1, b int default 2, c int not null default 0, primary key(c), index idx(b), index idx1(a), index idx2(b, c))")
	// Add some discrete rows.
	maxBatch := 20
	batchCnt := 100
	// Make sure there are no duplicate keys.
	defaultBatchSize := vardef.DefTiDBDDLReorgBatchSize * vardef.DefTiDBDDLReorgWorkerCount
	base := defaultBatchSize * 20
	for i := 1; i < batchCnt; i++ {
		n := base + i*defaultBatchSize + i
		for j := range rand.Intn(maxBatch) {
			n += j
			sql := fmt.Sprintf("insert into t3 values (%d, %d, %d)", n, n, n)
			tk.MustExec(sql)
		}
	}
	tk.MustExec("alter table t3 modify column a mediumint")
	tk.MustExec("admin check table t")

	// Test PointGet.
	tk.MustExec("create table t4(a bigint, b int, unique index idx(a));")
	tk.MustExec("insert into t4 values (1,1),(2,2),(3,3),(4,4),(5,5);")
	tk.MustExec("alter table t4 modify a bigint unsigned;")
	tk.MustQuery("select * from t4 where a=1;").Check(testkit.Rows("1 1"))

	// Test changing null to not null.
	tk.MustExec("create table t5(a bigint, b int, unique index idx(a));")
	tk.MustExec("insert into t5 values (1,1),(2,2),(3,3),(4,4),(5,5);")
	tk.MustExec("alter table t5 modify a int not null;")

	tk.MustExec("drop table t, t1, t2, t3, t4, t5")
}

func TestPartitionAddPanic(t *testing.T) {
	s := createFailDBSuite(t)
	tk := testkit.NewTestKit(t, s.store)
	tk.MustExec(`use test;`)
	tk.MustExec(`drop table if exists t;`)
	tk.MustExec(`create table t (a int) partition by range(a) (partition p0 values less than (10));`)
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/ddl/CheckPartitionByRangeErr", `return(true)`))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/ddl/CheckPartitionByRangeErr"))
	}()

	_, err := tk.Exec(`alter table t add partition (partition p1 values less than (20));`)
	require.Error(t, err)
	result := tk.MustQuery("show create table t").Rows()[0][1]
	require.Regexp(t, `PARTITION .p0. VALUES LESS THAN \(10\)`, result)
	require.NotRegexp(t, `PARTITION .p0. VALUES LESS THAN \(20\)`, result)
}
