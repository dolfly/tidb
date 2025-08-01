// Copyright 2015 PingCAP, Inc.
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

package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/grafana/pyroscope-go"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/pkg/bindinfo"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/config/kerneltype"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/executor"
	"github.com/pingcap/tidb/pkg/executor/mppcoordmanager"
	"github.com/pingcap/tidb/pkg/extension"
	_ "github.com/pingcap/tidb/pkg/extension/_import"
	"github.com/pingcap/tidb/pkg/keyspace"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	parsertypes "github.com/pingcap/tidb/pkg/parser/types"
	plannercore "github.com/pingcap/tidb/pkg/planner/core"
	"github.com/pingcap/tidb/pkg/plugin"
	"github.com/pingcap/tidb/pkg/privilege/privileges"
	"github.com/pingcap/tidb/pkg/resourcemanager"
	"github.com/pingcap/tidb/pkg/server"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/session/txninfo"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/standby"
	"github.com/pingcap/tidb/pkg/statistics"
	kvstore "github.com/pingcap/tidb/pkg/store"
	"github.com/pingcap/tidb/pkg/store/copr"
	"github.com/pingcap/tidb/pkg/store/driver"
	"github.com/pingcap/tidb/pkg/store/mockstore"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/cgmon"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/cpuprofile"
	"github.com/pingcap/tidb/pkg/util/deadlockhistory"
	"github.com/pingcap/tidb/pkg/util/disk"
	"github.com/pingcap/tidb/pkg/util/domainutil"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/kvcache"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/memory"
	"github.com/pingcap/tidb/pkg/util/metricsutil"
	"github.com/pingcap/tidb/pkg/util/naming"
	"github.com/pingcap/tidb/pkg/util/printer"
	"github.com/pingcap/tidb/pkg/util/redact"
	"github.com/pingcap/tidb/pkg/util/sem"
	"github.com/pingcap/tidb/pkg/util/signal"
	stmtsummaryv2 "github.com/pingcap/tidb/pkg/util/stmtsummary/v2"
	"github.com/pingcap/tidb/pkg/util/sys/linux"
	storageSys "github.com/pingcap/tidb/pkg/util/sys/storage"
	"github.com/pingcap/tidb/pkg/util/systimemon"
	"github.com/pingcap/tidb/pkg/util/tiflashcompute"
	"github.com/pingcap/tidb/pkg/util/topsql"
	"github.com/pingcap/tidb/pkg/util/versioninfo"
	repository "github.com/pingcap/tidb/pkg/util/workloadrepo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/txnkv/transaction"
	"go.uber.org/automaxprocs/maxprocs"
	"go.uber.org/zap"
)

// Flag Names
const (
	nmVersion          = "V"
	nmConfig           = "config"
	nmConfigCheck      = "config-check"
	nmConfigStrict     = "config-strict"
	nmStore            = "store"
	nmStorePath        = "path"
	nmHost             = "host"
	nmAdvertiseAddress = "advertise-address"
	nmPort             = "P"
	nmCors             = "cors"
	nmSocket           = "socket"
	nmRunDDL           = "run-ddl"
	nmLogLevel         = "L"
	nmLogFile          = "log-file"
	nmLogSlowQuery     = "log-slow-query"
	nmLogGeneral       = "log-general"
	nmReportStatus     = "report-status"
	nmStatusHost       = "status-host"
	nmStatusPort       = "status"
	nmMetricsAddr      = "metrics-addr"
	nmMetricsInterval  = "metrics-interval"
	nmDdlLease         = "lease"
	nmTokenLimit       = "token-limit"
	nmPluginDir        = "plugin-dir"
	nmPluginLoad       = "plugin-load"
	nmRepairMode       = "repair-mode"
	nmRepairList       = "repair-list"
	nmTempDir          = "temp-dir"

	nmRedact = "redact"

	nmProxyProtocolNetworks      = "proxy-protocol-networks"
	nmProxyProtocolHeaderTimeout = "proxy-protocol-header-timeout"
	nmProxyProtocolFallbackable  = "proxy-protocol-fallbackable"
	nmAffinityCPU                = "affinity-cpus"

	nmInitializeSecure            = "initialize-secure"
	nmInitializeInsecure          = "initialize-insecure"
	nmInitializeSQLFile           = "initialize-sql-file"
	nmDisconnectOnExpiredPassword = "disconnect-on-expired-password"
	nmKeyspaceName                = "keyspace-name"
	nmTiDBServiceScope            = "tidb-service-scope"

	nmStandby           = "standby"
	nmActivationTimeout = "activation-timeout"
	nmMaxIdleSeconds    = "max-idle-seconds"
)

var (
	version      *bool
	configPath   *string
	configCheck  *bool
	configStrict *bool

	// Base
	store            *string
	storePath        *string
	host             *string
	advertiseAddress *string
	port             *string
	cors             *string
	socket           *string
	enableBinlog     *bool
	runDDL           *bool
	ddlLease         *string
	tokenLimit       *int
	pluginDir        *string
	pluginLoad       *string
	affinityCPU      *string
	repairMode       *bool
	repairList       *string
	tempDir          *string

	// Log
	logLevel     *string
	logFile      *string
	logSlowQuery *string
	logGeneral   *string

	// Status
	reportStatus    *bool
	statusHost      *string
	statusPort      *string
	metricsAddr     *string
	metricsInterval *uint

	// subcommand collect-log
	redactFlag *bool

	// PROXY Protocol
	proxyProtocolNetworks      *string
	proxyProtocolHeaderTimeout *uint
	proxyProtocolFallbackable  *bool

	// Bootstrap and security
	initializeSecure            *bool
	initializeInsecure          *bool
	initializeSQLFile           *string
	disconnectOnExpiredPassword *bool
	keyspaceName                *string
	serviceScope                *string
	help                        *bool

	// Standby
	standbyMode       *bool
	activationTimeout *uint
	maxIdleSeconds    *uint
)

func initFlagSet() *flag.FlagSet {
	fset := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	version = flagBoolean(fset, nmVersion, false, "print version information and exit")
	configPath = fset.String(nmConfig, "", "config file path")
	configCheck = flagBoolean(fset, nmConfigCheck, false, "check config file validity and exit")
	configStrict = flagBoolean(fset, nmConfigStrict, false, "enforce config file validity")

	// Base
	store = fset.String(nmStore, string(config.StoreTypeUniStore), fmt.Sprintf("registered store name, %v", config.StoreTypeList()))
	storePath = fset.String(nmStorePath, "/tmp/tidb", "tidb storage path")
	host = fset.String(nmHost, "0.0.0.0", "tidb server host")
	advertiseAddress = fset.String(nmAdvertiseAddress, "", "tidb server advertise IP")
	port = fset.String(nmPort, "4000", "tidb server port")
	cors = fset.String(nmCors, "", "tidb server allow cors origin")
	socket = fset.String(nmSocket, "/tmp/tidb-{Port}.sock", "The socket file to use for connection.")
	runDDL = flagBoolean(fset, nmRunDDL, true, "run ddl worker on this tidb-server")
	ddlLease = fset.String(nmDdlLease, "45s", "schema lease duration, very dangerous to change only if you know what you do")
	tokenLimit = fset.Int(nmTokenLimit, 1000, "the limit of concurrent executed sessions")
	pluginDir = fset.String(nmPluginDir, "/data/deploy/plugin", "the folder that hold plugin")
	pluginLoad = fset.String(nmPluginLoad, "", "wait load plugin name(separated by comma)")
	affinityCPU = fset.String(nmAffinityCPU, "", "affinity cpu (cpu-no. separated by comma, e.g. 1,2,3)")
	repairMode = flagBoolean(fset, nmRepairMode, false, "enable admin repair mode")
	repairList = fset.String(nmRepairList, "", "admin repair table list")
	tempDir = fset.String(nmTempDir, config.DefTempDir, "tidb temporary directory")

	// Log
	logLevel = fset.String(nmLogLevel, "info", "log level: info, debug, warn, error, fatal")
	logFile = fset.String(nmLogFile, "", "log file path")
	logSlowQuery = fset.String(nmLogSlowQuery, "", "slow query file path")
	logGeneral = fset.String(nmLogGeneral, "", "general log file path")

	// Status
	reportStatus = flagBoolean(fset, nmReportStatus, true, "If enable status report HTTP service.")
	statusHost = fset.String(nmStatusHost, "0.0.0.0", "tidb server status host")
	statusPort = fset.String(nmStatusPort, "10080", "tidb server status port")
	metricsAddr = fset.String(nmMetricsAddr, "", "prometheus pushgateway address, leaves it empty will disable prometheus push.")
	metricsInterval = fset.Uint(nmMetricsInterval, 15, "prometheus client push interval in second, set \"0\" to disable prometheus push.")

	// subcommand collect-log
	redactFlag = flagBoolean(fset, nmRedact, false, "remove sensitive words from marked tidb logs when using collect-log subcommand, e.g. ./tidb-server --redact=xxx collect-log <input> <output>")

	// PROXY Protocol
	proxyProtocolNetworks = fset.String(nmProxyProtocolNetworks, "", "proxy protocol networks allowed IP or *, empty mean disable proxy protocol support")
	proxyProtocolHeaderTimeout = fset.Uint(nmProxyProtocolHeaderTimeout, 5, "proxy protocol header read timeout, unit is second. (Deprecated: as proxy protocol using lazy mode, header read timeout no longer used)")
	proxyProtocolFallbackable = flagBoolean(fset, nmProxyProtocolFallbackable, false, "enable proxy protocol fallback mode. If it is enabled, connection will return the client IP address when the client does not send PROXY Protocol Header and it will not return any error. (Note: This feature it does NOT follow the PROXY Protocol SPEC)")

	// Bootstrap and security
	initializeSecure = flagBoolean(fset, nmInitializeSecure, false, "bootstrap tidb-server in secure mode")
	initializeInsecure = flagBoolean(fset, nmInitializeInsecure, true, "bootstrap tidb-server in insecure mode")
	initializeSQLFile = fset.String(nmInitializeSQLFile, "", "SQL file to execute on first bootstrap")
	disconnectOnExpiredPassword = flagBoolean(fset, nmDisconnectOnExpiredPassword, true, "the server disconnects the client when the password is expired")
	keyspaceName = fset.String(nmKeyspaceName, "", "keyspace name.")
	serviceScope = fset.String(nmTiDBServiceScope, "", "tidb service scope")
	help = fset.Bool("help", false, "show the usage")

	// Standby
	standbyMode = flagBoolean(fset, nmStandby, false, "start tidb-server as standby")
	activationTimeout = fset.Uint(nmActivationTimeout, 0, "max time in second allowed for tidb to activate from standby, 0 means no limit")
	maxIdleSeconds = fset.Uint(nmMaxIdleSeconds, 0, "max idle seconds for a connection, 0 means no limit")

	session.RegisterMockUpgradeFlag(fset)
	// Ignore errors; CommandLine is set for ExitOnError.
	// nolint:errcheck
	fset.Parse(os.Args[1:])
	if *help {
		fset.Usage()
		os.Exit(0)
	}
	return fset
}

func main() {
	fset := initFlagSet()
	if args := fset.Args(); len(args) != 0 {
		if args[0] == "collect-log" && len(args) > 1 {
			output := "-"
			if len(args) > 2 {
				output = args[2]
			}
			terror.MustNil(redact.DeRedactFile(*redactFlag, args[1], output))
			return
		}
	}
	config.InitializeConfig(*configPath, *configCheck, *configStrict, overrideConfig, fset)
	if *version {
		setVersions()
		fmt.Println(printer.GetTiDBInfo())
		os.Exit(0)
	}
	// we cannot add this check inside config.Valid(), as previous '-V' also relies
	// on initialized global config.
	if kerneltype.IsNextGen() && len(config.GetGlobalConfig().KeyspaceName) == 0 && !config.GetGlobalConfig().Standby.StandByMode {
		fmt.Fprintln(os.Stderr, "invalid config: keyspace name or standby mode is required for nextgen TiDB")
		os.Exit(0)
	} else if kerneltype.IsClassic() && (len(config.GetGlobalConfig().KeyspaceName) > 0 || config.GetGlobalConfig().Standby.StandByMode) {
		fmt.Fprintln(os.Stderr, "invalid config: keyspace name or standby mode is not supported for classic TiDB")
		os.Exit(0)
	}

	var standbyController server.StandbyController
	if config.GetGlobalConfig().Standby.StandByMode {
		standbyController = standby.NewLoadKeyspaceController()
	}

	var err error

	// If running standby mode, wait for activate request.
	if standbyController != nil {
		standbyController.WaitForActivate()
		// EndStandby only execute once. If server is created
		// successfully, the defer has no effect. If panics
		// before server is created, the defer makes sure to
		// notify the activate caller.
		defer standbyController.EndStandby(err)
		// need to validate config again in case of config change via standby
		terror.MustNil(config.GetGlobalConfig().Valid())
	}

	signal.SetupUSR1Handler()
	err = registerStores()
	terror.MustNil(err)
	err = metricsutil.RegisterMetrics()
	terror.MustNil(err)

	if vardef.EnableTmpStorageOnOOM.Load() {
		config.GetGlobalConfig().UpdateTempStoragePath()
		err = disk.InitializeTempDir()
		terror.MustNil(err)
		err = checkTempStorageQuota()
		terror.MustNil(err)
	}
	err = setupLog()
	terror.MustNil(err)

	err = memory.InitMemoryHook()
	terror.MustNil(err)
	_, err = setupExtensions()
	terror.MustNil(err)
	setupStmtSummary()

	err = cpuprofile.StartCPUProfiler()
	terror.MustNil(err)

	if config.GetGlobalConfig().DisaggregatedTiFlash && config.GetGlobalConfig().UseAutoScaler {
		err = tiflashcompute.InitGlobalTopoFetcher(
			config.GetGlobalConfig().TiFlashComputeAutoScalerType,
			config.GetGlobalConfig().TiFlashComputeAutoScalerAddr,
			config.GetGlobalConfig().AutoScalerClusterID,
			config.GetGlobalConfig().IsTiFlashComputeFixedPool)
		terror.MustNil(err)
	}

	// Enable failpoints in tikv/client-go if the test API is enabled.
	// It appears in the main function to be set before any use of client-go to prevent data race.
	if _, err := failpoint.Status("github.com/pingcap/tidb/pkg/server/enableTestAPI"); err == nil {
		warnMsg := "tikv/client-go failpoint is enabled, this should NOT happen in the production environment"
		logutil.BgLogger().Warn(warnMsg)
		tikv.EnableFailpoints()
	}
	if intest.EnableInternalCheck {
		logutil.BgLogger().Warn("internal check is enabled, this should NOT happen in the production environment")
	}
	setGlobalVars()
	err = setCPUAffinity()
	terror.MustNil(err)
	cgmon.StartCgroupMonitor()
	err = setupTracing() // Should before createServer and after setup config.
	terror.MustNil(err)
	printInfo()
	setupMetrics()

	keyspaceName := keyspace.GetKeyspaceNameBySettings()
	executor.Start()
	resourcemanager.InstanceResourceManager.Start()
	storage, dom, err := createStoreDDLOwnerMgrAndDomain(keyspaceName)
	terror.MustNil(err)
	repository.SetupRepository(dom)
	svr := createServer(storage, dom)
	if standbyController != nil {
		standbyController.EndStandby(nil)

		svr.StandbyController = standbyController
		svr.StandbyController.OnServerCreated(svr)
	}

	exited := make(chan struct{})
	signal.SetupSignalHandler(func() {
		svr.Close()
		resourcemanager.InstanceResourceManager.Stop()
		cleanup(svr, storage, dom)
		cpuprofile.StopCPUProfiler()
		executor.Stop()
		close(exited)
	})
	topsql.SetupTopSQL(keyspace.GetKeyspaceNameBytesBySettings(), svr)
	terror.MustNil(svr.Run(dom))
	<-exited
	syncLog()
}

func syncLog() {
	if err := log.Sync(); err != nil {
		// Don't complain about /dev/stdout as Fsync will return EINVAL.
		if pathErr, ok := err.(*fs.PathError); ok {
			if pathErr.Path == "/dev/stdout" {
				os.Exit(0)
			}
		}
		fmt.Fprintln(os.Stderr, "sync log err:", err)
		os.Exit(1)
	}
}

func checkTempStorageQuota() error {
	// check capacity and the quota when EnableTmpStorageOnOOM is enabled
	c := config.GetGlobalConfig()
	if c.TempStorageQuota >= 0 {
		capacityByte, err := storageSys.GetTargetDirectoryCapacity(c.TempStoragePath)
		if err != nil {
			return err
		} else if capacityByte < uint64(c.TempStorageQuota) {
			return fmt.Errorf("value of [tmp-storage-quota](%d byte) exceeds the capacity(%d byte) of the [%s] directory", c.TempStorageQuota, capacityByte, c.TempStoragePath)
		}
	}
	return nil
}

func setCPUAffinity() error {
	if affinityCPU == nil || len(*affinityCPU) == 0 {
		return nil
	}
	var cpu []int
	for _, af := range strings.Split(*affinityCPU, ",") {
		af = strings.TrimSpace(af)
		if len(af) > 0 {
			c, err := strconv.Atoi(af)
			if err != nil {
				fmt.Fprintf(os.Stderr, "wrong affinity cpu config: %s", *affinityCPU)
				return err
			}
			cpu = append(cpu, c)
		}
	}
	err := linux.SetAffinity(cpu)
	if err != nil {
		fmt.Fprintf(os.Stderr, "set cpu affinity failure: %v", err)
		return err
	}
	if len(cpu) < runtime.GOMAXPROCS(0) {
		log.Info("cpu number less than maxprocs", zap.Int("cpu number ", len(cpu)), zap.Int("maxprocs", runtime.GOMAXPROCS(0)))
		runtime.GOMAXPROCS(len(cpu))
	}
	return nil
}

func registerStores() error {
	err := kvstore.Register(config.StoreTypeTiKV, &driver.TiKVDriver{})
	if err != nil {
		return err
	}
	err = kvstore.Register(config.StoreTypeMockTiKV, mockstore.MockTiKVDriver{})
	if err != nil {
		return err
	}
	err = kvstore.Register(config.StoreTypeUniStore, mockstore.EmbedUnistoreDriver{})
	return err
}

func createStoreDDLOwnerMgrAndDomain(keyspaceName string) (kv.Storage, *domain.Domain, error) {
	storage := kvstore.MustInitStorage(keyspaceName)
	if tikvStore, ok := storage.(kv.StorageWithPD); ok {
		pdhttpCli := tikvStore.GetPDHTTPClient()
		// unistore also implements kv.StorageWithPD, but it does not have PD client.
		if pdhttpCli != nil {
			pdStatus, err := pdhttpCli.GetStatus(context.Background())
			if err != nil {
				return nil, nil, err
			}
			if !kerneltype.IsMatch(pdStatus.KernelType) {
				log.Error("kernel type mismatch", zap.String("pd", pdStatus.KernelType),
					zap.String("tidb", kerneltype.Name()))
				return nil, nil, errors.New("kernel type mismatch")
			}
		}
	}
	copr.GlobalMPPFailedStoreProber.Run()
	mppcoordmanager.InstanceMPPCoordinatorManager.Run()
	// Bootstrap a session to load information schema.
	err := ddl.StartOwnerManager(context.Background(), storage)
	if err != nil {
		return nil, nil, err
	}
	dom, err := session.BootstrapSession(storage)
	if err != nil {
		return nil, nil, err
	}
	return storage, dom, nil
}

// Prometheus push.
const zeroDuration = time.Duration(0)

// pushMetric pushes metrics in background.
func pushMetric(addr string, interval time.Duration) {
	if interval == zeroDuration || len(addr) == 0 {
		log.Info("disable Prometheus push client")
		return
	}
	log.Info("start prometheus push client", zap.String("server addr", addr), zap.String("interval", interval.String()))
	go prometheusPushClient(addr, interval)
}

// prometheusPushClient pushes metrics to Prometheus Pushgateway.
func prometheusPushClient(addr string, interval time.Duration) {
	// TODO: TiDB do not have uniq name, so we use host+port to compose a name.
	job := "tidb"
	pusher := push.New(addr, job)
	pusher = pusher.Gatherer(prometheus.DefaultGatherer)
	pusher = pusher.Grouping("instance", instanceName())
	for {
		err := pusher.Push()
		if err != nil {
			log.Error("could not push metrics to prometheus pushgateway", zap.String("err", err.Error()))
		}
		time.Sleep(interval)
	}
}

func instanceName() string {
	cfg := config.GetGlobalConfig()
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%s_%d", hostname, cfg.Port)
}

// parseDuration parses lease argument string.
func parseDuration(lease string) time.Duration {
	dur, err := time.ParseDuration(lease)
	if err != nil {
		dur, err = time.ParseDuration(lease + "s")
	}
	if err != nil || dur < 0 {
		log.Fatal("invalid lease duration", zap.String("lease", lease))
	}
	return dur
}

func flagBoolean(fset *flag.FlagSet, name string, defaultVal bool, usage string) *bool {
	if !defaultVal {
		// Fix #4125, golang do not print default false value in usage, so we append it.
		usage = fmt.Sprintf("%s (default false)", usage)
		return fset.Bool(name, defaultVal, usage)
	}
	return fset.Bool(name, defaultVal, usage)
}

// overrideConfig considers command arguments and overrides some config items in the Config.
func overrideConfig(cfg *config.Config, fset *flag.FlagSet) {
	actualFlags := make(map[string]bool)
	fset.Visit(func(f *flag.Flag) {
		actualFlags[f.Name] = true
	})

	// Base
	if actualFlags[nmHost] {
		cfg.Host = *host
	}
	if actualFlags[nmAdvertiseAddress] {
		var err error
		if len(strings.Split(*advertiseAddress, " ")) > 1 {
			err = errors.Errorf("Only support one advertise-address")
		}
		terror.MustNil(err)
		cfg.AdvertiseAddress = *advertiseAddress
	}
	if len(cfg.AdvertiseAddress) == 0 && cfg.Host == "0.0.0.0" {
		cfg.AdvertiseAddress = util.GetLocalIP()
	}
	if len(cfg.AdvertiseAddress) == 0 {
		cfg.AdvertiseAddress = cfg.Host
	}
	var err error
	if actualFlags[nmPort] {
		var p int
		p, err = strconv.Atoi(*port)
		terror.MustNil(err)
		cfg.Port = uint(p)
	}
	if actualFlags[nmCors] {
		cfg.Cors = *cors
	}
	if actualFlags[nmStore] {
		cfg.Store = config.StoreType(*store)
	}
	if actualFlags[nmStorePath] {
		cfg.Path = *storePath
	}
	if actualFlags[nmSocket] {
		cfg.Socket = *socket
	}
	if actualFlags[nmRunDDL] {
		cfg.Instance.TiDBEnableDDL.Store(*runDDL)
	}
	if actualFlags[nmDdlLease] {
		cfg.Lease = *ddlLease
	}
	if actualFlags[nmTokenLimit] {
		cfg.TokenLimit = uint(*tokenLimit)
	}
	if actualFlags[nmPluginLoad] {
		cfg.Instance.PluginLoad = *pluginLoad
	}
	if actualFlags[nmPluginDir] {
		cfg.Instance.PluginDir = *pluginDir
	}

	if actualFlags[nmRepairMode] {
		cfg.RepairMode = *repairMode
	}
	if actualFlags[nmRepairList] {
		if cfg.RepairMode {
			cfg.RepairTableList = stringToList(*repairList)
		}
	}
	if actualFlags[nmTempDir] {
		cfg.TempDir = *tempDir
	}

	// Log
	if actualFlags[nmLogLevel] {
		cfg.Log.Level = *logLevel
	}
	if actualFlags[nmLogFile] {
		cfg.Log.File.Filename = *logFile
	}
	if actualFlags[nmLogSlowQuery] {
		cfg.Log.SlowQueryFile = *logSlowQuery
	}
	if actualFlags[nmLogGeneral] {
		cfg.Log.GeneralLogFile = *logGeneral
	}

	// Status
	if actualFlags[nmReportStatus] {
		cfg.Status.ReportStatus = *reportStatus
	}
	if actualFlags[nmStatusHost] {
		cfg.Status.StatusHost = *statusHost
	}
	if actualFlags[nmStatusPort] {
		var p int
		p, err = strconv.Atoi(*statusPort)
		terror.MustNil(err)
		cfg.Status.StatusPort = uint(p)
	}
	if actualFlags[nmMetricsAddr] {
		cfg.Status.MetricsAddr = *metricsAddr
	}
	if actualFlags[nmMetricsInterval] {
		cfg.Status.MetricsInterval = *metricsInterval
	}

	// PROXY Protocol
	if actualFlags[nmProxyProtocolNetworks] {
		cfg.ProxyProtocol.Networks = *proxyProtocolNetworks
	}
	if actualFlags[nmProxyProtocolHeaderTimeout] {
		cfg.ProxyProtocol.HeaderTimeout = *proxyProtocolHeaderTimeout
	}
	if actualFlags[nmProxyProtocolFallbackable] {
		cfg.ProxyProtocol.Fallbackable = *proxyProtocolFallbackable
	}

	// Sanity check: can't specify both options
	if actualFlags[nmInitializeSecure] && actualFlags[nmInitializeInsecure] {
		err = fmt.Errorf("the options -initialize-insecure and -initialize-secure are mutually exclusive")
		terror.MustNil(err)
	}
	// The option --initialize-secure=true ensures that a secure bootstrap is used.
	if actualFlags[nmInitializeSecure] {
		cfg.Security.SecureBootstrap = *initializeSecure
	}
	// The option --initialize-insecure=true/false was used.
	// Store the inverted value of this to the secure bootstrap cfg item
	if actualFlags[nmInitializeInsecure] {
		cfg.Security.SecureBootstrap = !*initializeInsecure
	}
	if actualFlags[nmDisconnectOnExpiredPassword] {
		cfg.Security.DisconnectOnExpiredPassword = *disconnectOnExpiredPassword
	}
	// Secure bootstrap initializes with Socket authentication
	// which is not supported on windows. Only the insecure bootstrap
	// method is supported.
	if runtime.GOOS == "windows" && cfg.Security.SecureBootstrap {
		err = fmt.Errorf("the option -initialize-secure is not supported on Windows")
		terror.MustNil(err)
	}
	// Initialize SQL File is used to run a set of SQL statements after first bootstrap.
	// It is important in the use case that you want to set GLOBAL variables, which
	// are persisted to the cluster and not read from a config file.
	if actualFlags[nmInitializeSQLFile] {
		if _, err := os.Stat(*initializeSQLFile); err != nil {
			err = fmt.Errorf("can not access -initialize-sql-file %s", *initializeSQLFile)
			terror.MustNil(err)
		}
		cfg.InitializeSQLFile = *initializeSQLFile
	}

	if actualFlags[nmKeyspaceName] {
		cfg.KeyspaceName = *keyspaceName
	}

	if actualFlags[nmTiDBServiceScope] {
		err = naming.Check(*serviceScope)
		terror.MustNil(err)
		cfg.Instance.TiDBServiceScope = *serviceScope
	}

	if actualFlags[nmStandby] {
		cfg.Standby.StandByMode = *standbyMode
	}

	if actualFlags[nmActivationTimeout] {
		cfg.Standby.ActivationTimeout = *activationTimeout
	}

	if actualFlags[nmMaxIdleSeconds] {
		cfg.Standby.MaxIdleSeconds = *maxIdleSeconds
	}
}

func setVersions() {
	cfg := config.GetGlobalConfig()
	if len(cfg.ServerVersion) > 0 {
		mysql.ServerVersion = cfg.ServerVersion
	}
	if len(cfg.TiDBEdition) > 0 {
		versioninfo.TiDBEdition = cfg.TiDBEdition
	}
	if len(cfg.TiDBReleaseVersion) > 0 {
		mysql.TiDBReleaseVersion = cfg.TiDBReleaseVersion
	}
}

func setGlobalVars() {
	cfg := config.GetGlobalConfig()

	// config.DeprecatedOptions records the config options that should be moved to [instance] section.
	for _, deprecatedOption := range config.DeprecatedOptions {
		for oldName := range deprecatedOption.NameMappings {
			switch deprecatedOption.SectionName {
			case "":
				switch oldName {
				case "check-mb4-value-in-utf8":
					cfg.Instance.CheckMb4ValueInUTF8.Store(cfg.CheckMb4ValueInUTF8.Load())
				case "enable-collect-execution-info":
					cfg.Instance.EnableCollectExecutionInfo.Store(cfg.EnableCollectExecutionInfo)
				case "max-server-connections":
					cfg.Instance.MaxConnections = cfg.MaxServerConnections
				case "run-ddl":
					cfg.Instance.TiDBEnableDDL.Store(cfg.RunDDL)
				}
			case "log":
				switch oldName {
				case "enable-slow-log":
					cfg.Instance.EnableSlowLog.Store(cfg.Log.EnableSlowLog.Load())
				case "slow-threshold":
					cfg.Instance.SlowThreshold = cfg.Log.SlowThreshold
				case "record-plan-in-slow-log":
					cfg.Instance.RecordPlanInSlowLog = cfg.Log.RecordPlanInSlowLog
				}
			case "performance":
				if oldName == "force-priority" {
					cfg.Instance.ForcePriority = cfg.Performance.ForcePriority
				}
			case "plugin":
				switch oldName {
				case "load":
					cfg.Instance.PluginLoad = cfg.Plugin.Load
				case "dir":
					cfg.Instance.PluginDir = cfg.Plugin.Dir
				}
			default:
			}
		}
	}

	// Disable automaxprocs log
	nopLog := func(string, ...any) {}
	_, err := maxprocs.Set(maxprocs.Logger(nopLog))
	terror.MustNil(err)
	// We should respect to user's settings in config file.
	// The default value of MaxProcs is 0, runtime.GOMAXPROCS(0) is no-op.
	runtime.GOMAXPROCS(int(cfg.Performance.MaxProcs))

	util.SetGOGC(cfg.Performance.GOGC)

	schemaLeaseDuration := parseDuration(cfg.Lease)
	if schemaLeaseDuration <= 0 {
		// previous version allow set schema lease to 0, and mainly used on
		// uni-store and for test, to be compatible we set it to default value here.
		log.Warn("schema lease is invalid, use default value",
			zap.String("lease", schemaLeaseDuration.String()))
		schemaLeaseDuration = config.DefSchemaLease
	}
	session.SetSchemaLease(schemaLeaseDuration)
	statsLeaseDuration := parseDuration(cfg.Performance.StatsLease)
	session.SetStatsLease(statsLeaseDuration)
	planReplayerGCLease := parseDuration(cfg.Performance.PlanReplayerGCLease)
	session.SetPlanReplayerGCLease(planReplayerGCLease)
	bindinfo.Lease = parseDuration(cfg.Performance.BindInfoLease)
	statistics.RatioOfPseudoEstimate.Store(cfg.Performance.PseudoEstimateRatio)
	if cfg.SplitTable {
		atomic.StoreUint32(&ddl.EnableSplitTableRegion, 1)
	}
	plannercore.AllowCartesianProduct.Store(cfg.Performance.CrossJoin)
	privileges.SkipWithGrant = cfg.Security.SkipGrantTable
	if cfg.Performance.TxnTotalSizeLimit == config.DefTxnTotalSizeLimit {
		// practically deprecate the config, let the new session memory tracker take charge of it.
		kv.TxnTotalSizeLimit.Store(config.SuperLargeTxnSize)
	} else {
		kv.TxnTotalSizeLimit.Store(cfg.Performance.TxnTotalSizeLimit)
	}
	if cfg.Performance.TxnEntrySizeLimit > config.MaxTxnEntrySizeLimit {
		log.Fatal("cannot set txn entry size limit larger than 120M")
	}
	kv.TxnEntrySizeLimit.Store(cfg.Performance.TxnEntrySizeLimit)

	priority := mysql.Str2Priority(cfg.Instance.ForcePriority)
	vardef.ForcePriority = int32(priority)

	vardef.ProcessGeneralLog.Store(cfg.Instance.TiDBGeneralLog)
	vardef.EnablePProfSQLCPU.Store(cfg.Instance.EnablePProfSQLCPU)
	vardef.EnableRCReadCheckTS.Store(cfg.Instance.TiDBRCReadCheckTS)
	vardef.IsSandBoxModeEnabled.Store(!cfg.Security.DisconnectOnExpiredPassword)
	atomic.StoreUint32(&vardef.DDLSlowOprThreshold, cfg.Instance.DDLSlowOprThreshold)
	atomic.StoreUint64(&vardef.ExpensiveQueryTimeThreshold, cfg.Instance.ExpensiveQueryTimeThreshold)
	atomic.StoreUint64(&vardef.ExpensiveTxnTimeThreshold, cfg.Instance.ExpensiveTxnTimeThreshold)

	if len(cfg.ServerVersion) > 0 {
		mysql.ServerVersion = cfg.ServerVersion
		variable.SetSysVar(vardef.Version, cfg.ServerVersion)
	}

	if len(cfg.TiDBEdition) > 0 {
		versioninfo.TiDBEdition = cfg.TiDBEdition
		variable.SetSysVar(vardef.VersionComment, "TiDB Server (Apache License 2.0) "+versioninfo.TiDBEdition+" Edition, MySQL 8.0 compatible")
	}
	if len(cfg.VersionComment) > 0 {
		variable.SetSysVar(vardef.VersionComment, cfg.VersionComment)
	}
	if len(cfg.TiDBReleaseVersion) > 0 {
		mysql.TiDBReleaseVersion = cfg.TiDBReleaseVersion
	}

	variable.SetSysVar(vardef.TiDBForcePriority, mysql.Priority2Str[priority])
	variable.SetSysVar(vardef.TiDBOptDistinctAggPushDown, variable.BoolToOnOff(cfg.Performance.DistinctAggPushDown))
	variable.SetSysVar(vardef.TiDBOptProjectionPushDown, variable.BoolToOnOff(cfg.Performance.ProjectionPushDown))
	variable.SetSysVar(vardef.Port, fmt.Sprintf("%d", cfg.Port))
	cfg.Socket = strings.Replace(cfg.Socket, "{Port}", fmt.Sprintf("%d", cfg.Port), 1)
	variable.SetSysVar(vardef.Socket, cfg.Socket)
	variable.SetSysVar(vardef.DataDir, cfg.Path)
	variable.SetSysVar(vardef.TiDBSlowQueryFile, cfg.Log.SlowQueryFile)
	variable.SetSysVar(vardef.TiDBIsolationReadEngines, strings.Join(cfg.IsolationRead.Engines, ","))
	variable.SetSysVar(vardef.TiDBEnforceMPPExecution, variable.BoolToOnOff(config.GetGlobalConfig().Performance.EnforceMPP))
	vardef.MemoryUsageAlarmRatio.Store(cfg.Instance.MemoryUsageAlarmRatio)
	variable.SetSysVar(vardef.TiDBConstraintCheckInPlacePessimistic, variable.BoolToOnOff(cfg.PessimisticTxn.ConstraintCheckInPlacePessimistic))
	if hostname, err := os.Hostname(); err == nil {
		variable.SetSysVar(vardef.Hostname, hostname)
	}
	vardef.GlobalLogMaxDays.Store(int32(config.GetGlobalConfig().Log.File.MaxDays))

	if cfg.Security.EnableSEM {
		sem.Enable()
	}

	// For CI environment we default enable prepare-plan-cache.
	if config.CheckTableBeforeDrop { // only for test
		variable.SetSysVar(vardef.TiDBEnablePrepPlanCache, variable.BoolToOnOff(true))
	}
	// use server-memory-quota as max-plan-cache-memory
	plannercore.PreparedPlanCacheMaxMemory.Store(cfg.Performance.ServerMemoryQuota)
	total, err := memory.MemTotal()
	terror.MustNil(err)
	// if server-memory-quota is larger than max-system-memory or not set, use max-system-memory as max-plan-cache-memory
	if plannercore.PreparedPlanCacheMaxMemory.Load() > total || plannercore.PreparedPlanCacheMaxMemory.Load() <= 0 {
		plannercore.PreparedPlanCacheMaxMemory.Store(total)
	}

	atomic.StoreUint64(&transaction.CommitMaxBackoff, uint64(parseDuration(cfg.TiKVClient.CommitTimeout).Seconds()*1000))
	tikv.SetRegionCacheTTLSec(int64(cfg.TiKVClient.RegionCacheTTL))
	domainutil.RepairInfo.SetRepairMode(cfg.RepairMode)
	domainutil.RepairInfo.SetRepairTableList(cfg.RepairTableList)
	executor.GlobalDiskUsageTracker.SetBytesLimit(cfg.TempStorageQuota)
	if cfg.Performance.ServerMemoryQuota < 1 {
		// If MaxMemory equals 0, it means unlimited
		executor.GlobalMemoryUsageTracker.SetBytesLimit(-1)
	} else {
		executor.GlobalMemoryUsageTracker.SetBytesLimit(int64(cfg.Performance.ServerMemoryQuota))
	}
	kvcache.GlobalLRUMemUsageTracker.AttachToGlobalTracker(executor.GlobalMemoryUsageTracker)

	t, err := time.ParseDuration(cfg.TiKVClient.StoreLivenessTimeout)
	if err != nil || t < 0 {
		logutil.BgLogger().Fatal("invalid duration value for store-liveness-timeout",
			zap.String("currentValue", cfg.TiKVClient.StoreLivenessTimeout))
	}
	tikv.SetStoreLivenessTimeout(t)
	parsertypes.TiDBStrictIntegerDisplayWidth = cfg.DeprecateIntegerDisplayWidth
	deadlockhistory.GlobalDeadlockHistory.Resize(cfg.PessimisticTxn.DeadlockHistoryCapacity)
	txninfo.Recorder.ResizeSummaries(cfg.TrxSummary.TransactionSummaryCapacity)
	txninfo.Recorder.SetMinDuration(time.Duration(cfg.TrxSummary.TransactionIDDigestMinDuration) * time.Millisecond)
	chunk.InitChunkAllocSize(cfg.TiDBMaxReuseChunk, cfg.TiDBMaxReuseColumn)

	if len(cfg.Instance.TiDBServiceScope) > 0 {
		vardef.ServiceScope.Store(strings.ToLower(cfg.Instance.TiDBServiceScope))
	}
}

func setupLog() error {
	cfg := config.GetGlobalConfig()
	err := logutil.InitLogger(cfg.Log.ToLogConfig(), keyspace.WrapZapcoreWithKeyspace())
	if err != nil {
		return err
	}

	// trigger internal http(s) client init.
	util.InternalHTTPClient()
	return nil
}

func setupExtensions() (*extension.Extensions, error) {
	err := extension.Setup()
	if err != nil {
		return nil, err
	}

	extensions, err := extension.GetExtensions()
	if err != nil {
		return nil, err
	}

	return extensions, nil
}

func printInfo() {
	// Make sure the TiDB info is always printed.
	level := log.GetLevel()
	log.SetLevel(zap.InfoLevel)
	printer.PrintTiDBInfo()
	log.SetLevel(level)
}

func createServer(storage kv.Storage, dom *domain.Domain) *server.Server {
	cfg := config.GetGlobalConfig()
	driver := server.NewTiDBDriver(storage)
	svr, err := server.NewServer(cfg, driver)
	// Both domain and storage have started, so we have to clean them before exiting.
	if err != nil {
		closeDDLOwnerMgrDomainAndStorage(storage, dom)
		log.Fatal("failed to create the server", zap.Error(err), zap.Stack("stack"))
	}
	svr.SetDomain(dom)
	go dom.ExpensiveQueryHandle().SetSessionManager(svr).Run()
	go dom.MemoryUsageAlarmHandle().SetSessionManager(svr).Run()
	go dom.ServerMemoryLimitHandle().SetSessionManager(svr).Run()
	dom.InfoSyncer().SetSessionManager(svr)
	return svr
}

func setupMetrics() {
	enablePyroscope()
	cfg := config.GetGlobalConfig()
	// Enable the mutex profile, 1/10 of mutex blocking event sampling.
	runtime.SetMutexProfileFraction(10)
	systimeErrHandler := func() {
		metrics.TimeJumpBackCounter.Inc()
	}
	go systimemon.StartMonitor(time.Now, systimeErrHandler)

	pushMetric(cfg.Status.MetricsAddr, time.Duration(cfg.Status.MetricsInterval)*time.Second)
}

func setupTracing() error {
	cfg := config.GetGlobalConfig()
	tracingCfg := cfg.OpenTracing.ToTracingConfig()
	tracingCfg.ServiceName = "TiDB"
	tracer, _, err := tracingCfg.NewTracer()
	if err != nil {
		log.Error("setup jaeger tracer failed", zap.String("error message", err.Error()))
		return err
	}
	opentracing.SetGlobalTracer(tracer)
	return nil
}

func closeDDLOwnerMgrDomainAndStorage(storage kv.Storage, dom *domain.Domain) {
	tikv.StoreShuttingDown(1)
	dom.Close()
	ddl.CloseOwnerManager()
	copr.GlobalMPPFailedStoreProber.Stop()
	mppcoordmanager.InstanceMPPCoordinatorManager.Stop()
	err := storage.Close()
	terror.Log(errors.Trace(err))
	if keyspace.IsRunningOnUser() {
		err = kvstore.GetSystemStorage().Close()
		terror.Log(errors.Annotate(err, "close system storage"))
	}
}

// The amount of time we wait for the ongoing txt to finished.
// We should better provider a dynamic way to set this value.
var gracefulCloseConnectionsTimeout = 15 * time.Second

func cleanup(svr *server.Server, storage kv.Storage, dom *domain.Domain) {
	dom.StopAutoAnalyze()

	drainClientWait := gracefulCloseConnectionsTimeout

	cancelClientWait := time.Second * 1
	svr.DrainClients(drainClientWait, cancelClientWait)

	// Kill sys processes such as auto analyze. Otherwise, tidb-server cannot exit until auto analyze is finished.
	// See https://github.com/pingcap/tidb/issues/40038 for details.
	svr.KillSysProcesses()
	plugin.Shutdown(context.Background())
	repository.StopRepository()
	closeDDLOwnerMgrDomainAndStorage(storage, dom)
	disk.CleanUp()
	closeStmtSummary()
	topsql.Close()
	cgmon.StopCgroupMonitor()
}

func stringToList(repairString string) []string {
	if len(repairString) <= 0 {
		return []string{}
	}
	if repairString[0] == '[' && repairString[len(repairString)-1] == ']' {
		repairString = repairString[1 : len(repairString)-1]
	}
	return strings.FieldsFunc(repairString, func(r rune) bool {
		return r == ',' || r == ' ' || r == '"'
	})
}

func setupStmtSummary() {
	instanceCfg := config.GetGlobalConfig().Instance
	if instanceCfg.StmtSummaryEnablePersistent {
		err := stmtsummaryv2.Setup(&stmtsummaryv2.Config{
			Filename:       instanceCfg.StmtSummaryFilename,
			FileMaxSize:    instanceCfg.StmtSummaryFileMaxSize,
			FileMaxDays:    instanceCfg.StmtSummaryFileMaxDays,
			FileMaxBackups: instanceCfg.StmtSummaryFileMaxBackups,
		})
		if err != nil {
			logutil.BgLogger().Error("failed to setup statements summary", zap.Error(err))
		}
	}
}

func closeStmtSummary() {
	instanceCfg := config.GetGlobalConfig().Instance
	if instanceCfg.StmtSummaryEnablePersistent {
		stmtsummaryv2.Close()
	}
}

func enablePyroscope() {
	if os.Getenv("PYROSCOPE_SERVER_ADDRESS") != "" {
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(5)
		_, err := pyroscope.Start(pyroscope.Config{
			ApplicationName:   "tidb",
			ServerAddress:     os.Getenv("PYROSCOPE_SERVER_ADDRESS"),
			Logger:            pyroscope.StandardLogger,
			AuthToken:         os.Getenv("PYROSCOPE_AUTH_TOKEN"),
			TenantID:          os.Getenv("PYROSCOPE_TENANT_ID"),
			BasicAuthUser:     os.Getenv("PYROSCOPE_BASIC_AUTH_USER"),
			BasicAuthPassword: os.Getenv("PYROSCOPE_BASIC_AUTH_PASSWORD"),
			ProfileTypes: []pyroscope.ProfileType{
				pyroscope.ProfileCPU,
				pyroscope.ProfileAllocSpace,
			},
			UploadRate: 30 * time.Second,
		})
		if err != nil {
			log.Fatal("fail to start pyroscope", zap.Error(err))
		}
	}
}
