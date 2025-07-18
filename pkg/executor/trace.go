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

package executor

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opentracing/basictracer-go"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/planner/core"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
	"sourcegraph.com/sourcegraph/appdash"
	traceImpl "sourcegraph.com/sourcegraph/appdash/opentracing"
)

// TraceExec represents a root executor of trace query.
type TraceExec struct {
	exec.BaseExecutor
	// CollectedSpans collects all span during execution. Span is appended via
	// callback method which passes into tracer implementation.
	CollectedSpans []basictracer.RawSpan
	// exhausted being true means there is no more result.
	exhausted bool
	// stmtNode is the real query ast tree and it is used for building real query's plan.
	stmtNode   ast.StmtNode
	resolveCtx *resolve.Context

	builder *executorBuilder
	format  string

	// optimizerTrace indicates 'trace plan statement'
	optimizerTrace       bool
	optimizerTraceTarget string
}

// Next executes real query and collects span later.
func (e *TraceExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.Reset()
	if e.exhausted {
		return nil
	}
	se := e.Ctx().GetSQLExecutor()
	// For audit log plugin to set the correct statement.
	stmtCtx := e.Ctx().GetSessionVars().StmtCtx
	defer func() {
		e.Ctx().GetSessionVars().StmtCtx = stmtCtx
	}()

	if e.optimizerTrace {
		switch e.optimizerTraceTarget {
		case core.TracePlanTargetEstimation:
			return e.nextOptimizerCEPlanTrace(ctx, e.Ctx(), req)
		case core.TracePlanTargetDebug:
			return e.nextOptimizerDebugPlanTrace(ctx, e.Ctx(), req)
		default:
			return e.nextOptimizerPlanTrace(ctx, e.Ctx(), req)
		}
	}

	ctx = util.ContextWithTraceExecDetails(ctx)
	switch e.format {
	case core.TraceFormatLog:
		return e.nextTraceLog(ctx, se, req)
	default:
		return e.nextRowJSON(ctx, se, req)
	}
}

func (e *TraceExec) nextOptimizerCEPlanTrace(ctx context.Context, se sessionctx.Context, req *chunk.Chunk) error {
	stmtCtx := se.GetSessionVars().StmtCtx
	origin := stmtCtx.EnableOptimizerCETrace
	stmtCtx.EnableOptimizerCETrace = true
	defer func() {
		stmtCtx.EnableOptimizerCETrace = origin
	}()

	nodeW := resolve.NewNodeWWithCtx(e.stmtNode, e.resolveCtx)
	_, _, err := core.OptimizeAstNodeNoCache(ctx, se, nodeW, se.GetInfoSchema().(infoschema.InfoSchema))
	if err != nil {
		return err
	}

	writer := strings.Builder{}
	jsonEncoder := json.NewEncoder(&writer)
	// If we do not set this to false, ">", "<", "&"... will be escaped to "\u003c","\u003e", "\u0026"...
	jsonEncoder.SetEscapeHTML(false)
	err = jsonEncoder.Encode(stmtCtx.OptimizerCETrace)
	if err != nil {
		return errors.AddStack(err)
	}
	res := []byte(writer.String())

	req.AppendBytes(0, res)
	e.exhausted = true
	return nil
}

func (e *TraceExec) nextOptimizerDebugPlanTrace(ctx context.Context, se sessionctx.Context, req *chunk.Chunk) error {
	stmtCtx := se.GetSessionVars().StmtCtx
	origin := stmtCtx.EnableOptimizerDebugTrace
	stmtCtx.EnableOptimizerDebugTrace = true
	defer func() {
		stmtCtx.EnableOptimizerDebugTrace = origin
	}()

	nodeW := resolve.NewNodeWWithCtx(e.stmtNode, e.resolveCtx)
	_, _, err := core.OptimizeAstNodeNoCache(ctx, se, nodeW, se.GetInfoSchema().(infoschema.InfoSchema))
	if err != nil {
		return err
	}

	writer := strings.Builder{}
	jsonEncoder := json.NewEncoder(&writer)
	// If we do not set this to false, ">", "<", "&"... will be escaped to "\u003c","\u003e", "\u0026"...
	jsonEncoder.SetEscapeHTML(false)
	err = jsonEncoder.Encode(stmtCtx.OptimizerDebugTrace)
	if err != nil {
		return errors.AddStack(err)
	}
	res := []byte(writer.String())

	req.AppendBytes(0, res)
	e.exhausted = true
	return nil
}

func (e *TraceExec) nextOptimizerPlanTrace(ctx context.Context, se sessionctx.Context, req *chunk.Chunk) error {
	zf, fileName, err := generateOptimizerTraceFile()
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)
	defer func() {
		err := zw.Close()
		if err != nil {
			logutil.BgLogger().Warn("Closing zip writer failed", zap.Error(err))
		}
		err = zf.Close()
		if err != nil {
			logutil.BgLogger().Warn("Closing zip file failed", zap.Error(err))
		}
	}()
	traceZW, err := zw.Create("trace.json")
	if err != nil {
		return errors.AddStack(err)
	}
	stmtCtx := se.GetSessionVars().StmtCtx
	origin := stmtCtx.EnableOptimizeTrace
	stmtCtx.EnableOptimizeTrace = true
	defer func() {
		stmtCtx.EnableOptimizeTrace = origin
	}()
	nodeW := resolve.NewNodeWWithCtx(e.stmtNode, e.resolveCtx)
	_, _, err = core.OptimizeAstNodeNoCache(ctx, se, nodeW, se.GetInfoSchema().(infoschema.InfoSchema))
	if err != nil {
		return err
	}

	writer := strings.Builder{}
	jsonEncoder := json.NewEncoder(&writer)
	// If we do not set this to false, ">", "<", "&"... will be escaped to "\u003c","\u003e", "\u0026"...
	jsonEncoder.SetEscapeHTML(false)
	err = jsonEncoder.Encode(se.GetSessionVars().StmtCtx.OptimizeTracer)
	if err != nil {
		return errors.AddStack(err)
	}
	res := []byte(writer.String())

	_, err = traceZW.Write(res)
	if err != nil {
		return errors.AddStack(err)
	}
	req.AppendString(0, fileName)
	e.exhausted = true
	return nil
}

func (e *TraceExec) nextTraceLog(ctx context.Context, se sqlexec.SQLExecutor, req *chunk.Chunk) error {
	recorder := basictracer.NewInMemoryRecorder()
	tracer := basictracer.New(recorder)
	span := tracer.StartSpan("trace")
	ctx = opentracing.ContextWithSpan(ctx, span)

	e.executeChild(ctx, se)
	span.Finish()

	generateLogResult(recorder.GetSpans(), req)
	e.exhausted = true
	return nil
}

func (e *TraceExec) nextRowJSON(ctx context.Context, se sqlexec.SQLExecutor, req *chunk.Chunk) error {
	store := appdash.NewMemoryStore()
	tracer := traceImpl.NewTracer(store)
	span := tracer.StartSpan("trace")
	ctx = opentracing.ContextWithSpan(ctx, span)

	e.executeChild(ctx, se)
	span.Finish()

	traces, err := store.Traces(appdash.TracesOpts{})
	if err != nil {
		return errors.Trace(err)
	}

	// Row format.
	if e.format != core.TraceFormatJSON {
		if len(traces) < 1 {
			e.exhausted = true
			return nil
		}
		trace := traces[0]
		dfsTree(trace, "", false, req)
		e.exhausted = true
		return nil
	}

	// Json format.
	data, err := json.Marshal(traces)
	if err != nil {
		return errors.Trace(err)
	}

	// Split json data into rows to avoid the max packet size limitation.
	const maxRowLen = 4096
	for len(data) > maxRowLen {
		req.AppendString(0, string(data[:maxRowLen]))
		data = data[maxRowLen:]
	}
	req.AppendString(0, string(data))
	e.exhausted = true
	return nil
}

func (e *TraceExec) executeChild(ctx context.Context, se sqlexec.SQLExecutor) {
	// For audit log plugin to log the statement correctly.
	// Should be logged as 'explain ...', instead of the executed SQL.
	vars := e.Ctx().GetSessionVars()
	origin := vars.InRestrictedSQL
	vars.InRestrictedSQL = true
	defer func() {
		vars.InRestrictedSQL = origin
	}()
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnTrace)
	rs, err := se.ExecuteStmt(ctx, e.stmtNode)
	if err != nil {
		var errCode uint16
		if te, ok := err.(*terror.Error); ok {
			errCode = terror.ToSQLError(te).Code
		}
		logutil.Eventf(ctx, "execute with error(%d): %s", errCode, err.Error())
	}
	if rs != nil {
		drainRecordSet(ctx, e.Ctx(), rs)
		if err = rs.Close(); err != nil {
			logutil.Logger(ctx).Error("run trace close result with error", zap.Error(err))
		}
	}
	logutil.Eventf(ctx, "execute done, modify row: %d", e.Ctx().GetSessionVars().StmtCtx.AffectedRows())
}

func drainRecordSet(ctx context.Context, sctx sessionctx.Context, rs sqlexec.RecordSet) {
	req := rs.NewChunk(nil)
	var rowCount int
	for {
		err := rs.Next(ctx, req)
		if err != nil || req.NumRows() == 0 {
			if err != nil {
				var errCode uint16
				if te, ok := err.(*terror.Error); ok {
					errCode = terror.ToSQLError(te).Code
				}
				logutil.Eventf(ctx, "execute with error(%d): %s", errCode, err.Error())
			} else {
				logutil.Eventf(ctx, "execute done, ReturnRow: %d, ModifyRow: %d", rowCount, sctx.GetSessionVars().StmtCtx.AffectedRows())
			}
			return
		}
		rowCount += req.NumRows()
		req.Reset()
	}
}

func dfsTree(t *appdash.Trace, prefix string, isLast bool, chk *chunk.Chunk) {
	var newPrefix, suffix string
	if prefix == "" {
		newPrefix = prefix + "  "
	} else {
		if !isLast {
			suffix = "├─"
			newPrefix = prefix + "│ "
		} else {
			suffix = "└─"
			newPrefix = prefix + "  "
		}
	}

	var start time.Time
	var duration time.Duration
	if e, err := t.TimespanEvent(); err == nil {
		start = e.Start()
		end := e.End()
		duration = end.Sub(start)
	}

	chk.AppendString(0, prefix+suffix+t.Span.Name())
	chk.AppendString(1, start.Format("15:04:05.000000"))
	chk.AppendString(2, duration.String())

	// Sort events by their start time
	slices.SortFunc(t.Sub, func(i, j *appdash.Trace) int {
		var istart, jstart time.Time
		if ievent, err := i.TimespanEvent(); err == nil {
			istart = ievent.Start()
		}
		if jevent, err := j.TimespanEvent(); err == nil {
			jstart = jevent.Start()
		}
		return istart.Compare(jstart)
	})

	for i, sp := range t.Sub {
		dfsTree(sp, newPrefix, i == (len(t.Sub))-1 /*last element of array*/, chk)
	}
}

func generateLogResult(allSpans []basictracer.RawSpan, chk *chunk.Chunk) {
	for rIdx := range allSpans {
		span := &allSpans[rIdx]

		chk.AppendTime(0, types.NewTime(types.FromGoTime(span.Start), mysql.TypeTimestamp, 6))
		chk.AppendString(1, "--- start span "+span.Operation+" ----")
		chk.AppendString(2, "")
		chk.AppendString(3, span.Operation)

		var tags string
		if len(span.Tags) > 0 {
			tags = fmt.Sprintf("%v", span.Tags)
		}
		for _, l := range span.Logs {
			for _, field := range l.Fields {
				if field.Key() == logutil.TraceEventKey {
					chk.AppendTime(0, types.NewTime(types.FromGoTime(l.Timestamp), mysql.TypeTimestamp, 6))
					chk.AppendString(1, field.Value().(string))
					chk.AppendString(2, tags)
					chk.AppendString(3, span.Operation)
				}
			}
		}
	}
}

func generateOptimizerTraceFile() (*os.File, string, error) {
	dirPath := domain.GetOptimizerTraceDirName()
	// Create path
	err := os.MkdirAll(dirPath, os.ModePerm)
	if err != nil {
		return nil, "", errors.AddStack(err)
	}
	// Generate key and create zip file
	time := time.Now().UnixNano()
	b := make([]byte, 16)
	//nolint: gosec
	_, err = rand.Read(b)
	if err != nil {
		return nil, "", errors.AddStack(err)
	}
	key := base64.URLEncoding.EncodeToString(b)
	fileName := fmt.Sprintf("optimizer_trace_%v_%v.zip", key, time)
	zf, err := os.Create(filepath.Join(dirPath, fileName))
	if err != nil {
		return nil, "", errors.AddStack(err)
	}
	return zf, fileName, nil
}
