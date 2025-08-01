// Copyright 2020 PingCAP, Inc.
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

package unistore

import (
	"context"
	"io"
	"math"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/debugpb"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/mpp"
	"github.com/pingcap/tidb/pkg/parser/terror"
	us "github.com/pingcap/tidb/pkg/store/mockstore/unistore/tikv"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util/async"
	"google.golang.org/grpc/metadata"
)

// For gofail injection.
var undeterminedErr = terror.ErrResultUndetermined

// RPCClient sends kv RPC calls to mock cluster. RPCClient mocks the behavior of
// a rpc client at tikv's side.
type RPCClient struct {
	usSvr      *us.Server
	cluster    *Cluster
	path       string
	rawHandler *rawHandler
	persistent bool
	closed     int32
}

// CheckResourceTagForTopSQLInGoTest is used to identify whether check resource tag for TopSQL.
var CheckResourceTagForTopSQLInGoTest bool

// UnistoreRPCClientSendHook exports for test.
var UnistoreRPCClientSendHook atomic.Pointer[func(*tikvrpc.Request)]

// SendRequestAsync sends a request to mock cluster asynchronously.
func (c *RPCClient) SendRequestAsync(ctx context.Context, addr string, req *tikvrpc.Request, cb async.Callback[*tikvrpc.Response]) {
	go func() {
		cb.Schedule(c.SendRequest(ctx, addr, req, tikv.ReadTimeoutMedium))
	}()
}

// SendRequest sends a request to mock cluster.
func (c *RPCClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	tikvrpc.AttachContext(req, req.Context)

	failpoint.Inject("rpcServerBusy", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(tikvrpc.GenRegionErrorResp(req, &errorpb.Error{ServerIsBusy: &errorpb.ServerIsBusy{}}))
		}
	})
	failpoint.Inject("epochNotMatch", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(tikvrpc.GenRegionErrorResp(req, &errorpb.Error{EpochNotMatch: &errorpb.EpochNotMatch{}}))
		}
	})

	failpoint.Inject("unistoreRPCClientSendHook", func(val failpoint.Value) {
		if fn := UnistoreRPCClientSendHook.Load(); val.(bool) && fn != nil {
			(*fn)(req)
		}
	})

	failpoint.Inject("rpcTiKVAllowedOnAlmostFull", func(val failpoint.Value) {
		if val.(bool) {
			if req.Type == tikvrpc.CmdPrewrite || req.Type == tikvrpc.CmdCommit {
				if req.Context.DiskFullOpt != kvrpcpb.DiskFullOpt_AllowedOnAlmostFull {
					failpoint.Return(tikvrpc.GenRegionErrorResp(req, &errorpb.Error{DiskFull: &errorpb.DiskFull{StoreId: []uint64{1}, Reason: "disk full"}}))
				}
			}
		}
	})
	failpoint.Inject("unistoreRPCDeadlineExceeded", func(val failpoint.Value) {
		if val.(bool) && timeout < time.Second {
			failpoint.Return(tikvrpc.GenRegionErrorResp(req, &errorpb.Error{Message: "Deadline is exceeded"}))
		}
	})
	failpoint.Inject("unistoreRPCSlowByInjestSleep", func(val failpoint.Value) {
		time.Sleep(time.Duration(val.(int) * int(time.Millisecond)))
		failpoint.Return(tikvrpc.GenRegionErrorResp(req, &errorpb.Error{Message: "Deadline is exceeded"}))
	})
	failpoint.Inject("unistoreRPCSlowCop", func(val failpoint.Value) {
		if req.Type == tikvrpc.CmdCop {
			time.Sleep(time.Duration(val.(int) * int(time.Millisecond)))
		}
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if atomic.LoadInt32(&c.closed) != 0 {
		// Return `context.Canceled` can break Backoff.
		return nil, context.Canceled
	}

	storeID, err := c.usSvr.GetStoreIDByAddr(addr)
	if err != nil {
		return nil, err
	}

	if CheckResourceTagForTopSQLInGoTest {
		err = checkResourceTagForTopSQL(req)
		if err != nil {
			return nil, err
		}
	}

	resp := &tikvrpc.Response{}
	switch req.Type {
	case tikvrpc.CmdGet:
		resp.Resp, err = c.usSvr.KvGet(ctx, req.Get())
	case tikvrpc.CmdScan:
		kvScanReq := req.Scan()
		failpoint.Inject("rpcScanResult", func(val failpoint.Value) {
			switch val.(string) {
			case "keyError":
				failpoint.Return(&tikvrpc.Response{
					Resp: &kvrpcpb.ScanResponse{Error: &kvrpcpb.KeyError{
						Locked: &kvrpcpb.LockInfo{
							PrimaryLock: kvScanReq.StartKey,
							LockVersion: kvScanReq.Version - 1,
							Key:         kvScanReq.StartKey,
							LockTtl:     50,
							TxnSize:     1,
							LockType:    kvrpcpb.Op_Put,
						},
					}},
				}, nil)
			}
		})

		resp.Resp, err = c.usSvr.KvScan(ctx, kvScanReq)
	case tikvrpc.CmdPrewrite:
		failpoint.Inject("rpcPrewriteResult", func(val failpoint.Value) {
			if val != nil {
				switch val.(string) {
				case "timeout":
					failpoint.Return(nil, errors.New("timeout"))
				case "notLeader":
					failpoint.Return(&tikvrpc.Response{
						Resp: &kvrpcpb.PrewriteResponse{RegionError: &errorpb.Error{NotLeader: &errorpb.NotLeader{}}},
					}, nil)
				case "writeConflict":
					failpoint.Return(&tikvrpc.Response{
						Resp: &kvrpcpb.PrewriteResponse{Errors: []*kvrpcpb.KeyError{{Conflict: &kvrpcpb.WriteConflict{}}}},
					}, nil)
				}
			}
		})

		r := req.Prewrite()
		c.cluster.handleDelay(r.StartVersion, r.Context.RegionId)
		resp.Resp, err = c.usSvr.KvPrewrite(ctx, r)

		failpoint.Inject("rpcPrewriteTimeout", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(nil, undeterminedErr)
			}
		})
	case tikvrpc.CmdPessimisticLock:
		r := req.PessimisticLock()
		c.cluster.handleDelay(r.StartVersion, r.Context.RegionId)
		resp.Resp, err = c.usSvr.KvPessimisticLock(ctx, r)
	case tikvrpc.CmdPessimisticRollback:
		resp.Resp, err = c.usSvr.KVPessimisticRollback(ctx, req.PessimisticRollback())
	case tikvrpc.CmdCommit:
		failpoint.Inject("rpcCommitResult", func(val failpoint.Value) {
			switch val.(string) {
			case "timeout":
				failpoint.Return(nil, errors.New("timeout"))
			case "notLeader":
				failpoint.Return(&tikvrpc.Response{
					Resp: &kvrpcpb.CommitResponse{RegionError: &errorpb.Error{NotLeader: &errorpb.NotLeader{}}},
				}, nil)
			case "keyError":
				failpoint.Return(&tikvrpc.Response{
					Resp: &kvrpcpb.CommitResponse{Error: &kvrpcpb.KeyError{}},
				}, nil)
			case "undeterminedResult":
				failpoint.Return(&tikvrpc.Response{
					Resp: &kvrpcpb.CommitResponse{RegionError: &errorpb.Error{
						UndeterminedResult: &errorpb.UndeterminedResult{}},
					},
				}, nil)
			}
		})

		resp.Resp, err = c.usSvr.KvCommit(ctx, req.Commit())

		failpoint.Inject("rpcCommitTimeout", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(nil, undeterminedErr)
			}
		})
	case tikvrpc.CmdCleanup:
		resp.Resp, err = c.usSvr.KvCleanup(ctx, req.Cleanup())
	case tikvrpc.CmdCheckTxnStatus:
		resp.Resp, err = c.usSvr.KvCheckTxnStatus(ctx, req.CheckTxnStatus())
	case tikvrpc.CmdCheckSecondaryLocks:
		resp.Resp, err = c.usSvr.KvCheckSecondaryLocks(ctx, req.CheckSecondaryLocks())
	case tikvrpc.CmdTxnHeartBeat:
		resp.Resp, err = c.usSvr.KvTxnHeartBeat(ctx, req.TxnHeartBeat())
	case tikvrpc.CmdBatchGet:
		batchGetReq := req.BatchGet()
		failpoint.Inject("rpcBatchGetResult", func(val failpoint.Value) {
			switch val.(string) {
			case "keyError":
				failpoint.Return(&tikvrpc.Response{
					Resp: &kvrpcpb.BatchGetResponse{Error: &kvrpcpb.KeyError{
						Locked: &kvrpcpb.LockInfo{
							PrimaryLock: batchGetReq.Keys[0],
							LockVersion: batchGetReq.Version - 1,
							Key:         batchGetReq.Keys[0],
							LockTtl:     50,
							TxnSize:     1,
							LockType:    kvrpcpb.Op_Put,
						},
					}},
				}, nil)
			}
		})

		resp.Resp, err = c.usSvr.KvBatchGet(ctx, batchGetReq)
	case tikvrpc.CmdBatchRollback:
		resp.Resp, err = c.usSvr.KvBatchRollback(ctx, req.BatchRollback())
	case tikvrpc.CmdScanLock:
		resp.Resp, err = c.usSvr.KvScanLock(ctx, req.ScanLock())
	case tikvrpc.CmdResolveLock:
		resp.Resp, err = c.usSvr.KvResolveLock(ctx, req.ResolveLock())
	case tikvrpc.CmdGC:
		resp.Resp, err = c.usSvr.KvGC(ctx, req.GC())
	case tikvrpc.CmdDeleteRange:
		resp.Resp, err = c.usSvr.KvDeleteRange(ctx, req.DeleteRange())
	case tikvrpc.CmdRawGet:
		resp.Resp, err = c.rawHandler.RawGet(ctx, req.RawGet())
	case tikvrpc.CmdRawBatchGet:
		resp.Resp, err = c.rawHandler.RawBatchGet(ctx, req.RawBatchGet())
	case tikvrpc.CmdRawPut:
		resp.Resp, err = c.rawHandler.RawPut(ctx, req.RawPut())
	case tikvrpc.CmdRawBatchPut:
		resp.Resp, err = c.rawHandler.RawBatchPut(ctx, req.RawBatchPut())
	case tikvrpc.CmdRawDelete:
		resp.Resp, err = c.rawHandler.RawDelete(ctx, req.RawDelete())
	case tikvrpc.CmdRawBatchDelete:
		resp.Resp, err = c.rawHandler.RawBatchDelete(ctx, req.RawBatchDelete())
	case tikvrpc.CmdRawDeleteRange:
		resp.Resp, err = c.rawHandler.RawDeleteRange(ctx, req.RawDeleteRange())
	case tikvrpc.CmdRawScan:
		resp.Resp, err = c.rawHandler.RawScan(ctx, req.RawScan())
	case tikvrpc.CmdCop:
		resp.Resp, err = c.usSvr.Coprocessor(ctx, req.Cop())
	case tikvrpc.CmdCopStream:
		resp.Resp, err = c.handleCopStream(ctx, req.Cop())
	case tikvrpc.CmdBatchCop:
		failpoint.Inject("BatchCopCancelled", func(value failpoint.Value) {
			if value.(bool) {
				failpoint.Return(nil, context.Canceled)
			}
		})

		failpoint.Inject("BatchCopRpcErr"+addr, func(value failpoint.Value) {
			if value.(string) == addr {
				failpoint.Return(nil, errors.New("rpc error"))
			}
		})
		resp.Resp, err = c.handleBatchCop(ctx, req.BatchCop(), timeout)
	case tikvrpc.CmdMPPConn:
		failpoint.Inject("mppConnTimeout", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(nil, errors.New("rpc error"))
			}
		})
		failpoint.Inject("MppVersionError", func(val failpoint.Value) {
			if v := int64(val.(int)); v > req.EstablishMPPConn().GetReceiverMeta().GetMppVersion() || v > req.EstablishMPPConn().GetSenderMeta().GetMppVersion() {
				failpoint.Return(nil, context.Canceled)
			}
		})
		resp.Resp, err = c.handleEstablishMPPConnection(ctx, req.EstablishMPPConn(), timeout, storeID)
	case tikvrpc.CmdMPPTask:
		failpoint.Inject("mppDispatchTimeout", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(nil, errors.New("rpc error"))
			}
		})
		failpoint.Inject("MppVersionError", func(val failpoint.Value) {
			if v := int64(val.(int)); v > req.DispatchMPPTask().GetMeta().GetMppVersion() {
				failpoint.Return(nil, context.Canceled)
			}
		})
		resp.Resp, err = c.handleDispatchMPPTask(ctx, req.DispatchMPPTask(), storeID)
	case tikvrpc.CmdMPPCancel:
	case tikvrpc.CmdMvccGetByKey:
		resp.Resp, err = c.usSvr.MvccGetByKey(ctx, req.MvccGetByKey())
	case tikvrpc.CmdMPPAlive:
		resp.Resp, err = c.usSvr.IsAlive(ctx, req.IsMPPAlive())
	case tikvrpc.CmdMvccGetByStartTs:
		resp.Resp, err = c.usSvr.MvccGetByStartTs(ctx, req.MvccGetByStartTs())
	case tikvrpc.CmdSplitRegion:
		resp.Resp, err = c.usSvr.SplitRegion(ctx, req.SplitRegion())
	case tikvrpc.CmdDebugGetRegionProperties:
		resp.Resp, err = c.handleDebugGetRegionProperties(ctx, req.DebugGetRegionProperties())
		return resp, err
	case tikvrpc.CmdStoreSafeTS:
		resp.Resp, err = c.usSvr.GetStoreSafeTS(ctx, req.StoreSafeTS())
		return resp, err
	case tikvrpc.CmdUnsafeDestroyRange:
		// Pretend it was done. Unistore does not have "destroy", and the
		// keys has already been removed one-by-one before through:
		// (dr *delRange) startEmulator()
		resp.Resp = &kvrpcpb.UnsafeDestroyRangeResponse{}
		return resp, nil
	case tikvrpc.CmdFlush:
		r := req.Flush()
		c.cluster.handleDelay(r.StartTs, r.Context.RegionId)
		resp.Resp, err = c.usSvr.KvFlush(ctx, r)
	case tikvrpc.CmdBufferBatchGet:
		r := req.BufferBatchGet()
		resp.Resp, err = c.usSvr.KvBufferBatchGet(ctx, r)
	default:
		err = errors.Errorf("not support this request type %v", req.Type)
	}
	if err != nil {
		return nil, err
	}
	var regErr *errorpb.Error
	if req.Type != tikvrpc.CmdBatchCop && req.Type != tikvrpc.CmdMPPConn && req.Type != tikvrpc.CmdMPPTask && req.Type != tikvrpc.CmdMPPAlive {
		regErr, err = resp.GetRegionError()
	}
	if err != nil {
		return nil, err
	}
	if regErr != nil {
		if regErr.EpochNotMatch != nil {
			for i, newReg := range regErr.EpochNotMatch.CurrentRegions {
				regErr.EpochNotMatch.CurrentRegions[i] = proto.Clone(newReg).(*metapb.Region)
			}
		}
	}
	return resp, nil
}

func (c *RPCClient) handleCopStream(ctx context.Context, req *coprocessor.Request) (*tikvrpc.CopStreamResponse, error) {
	copResp, err := c.usSvr.Coprocessor(ctx, req)
	if err != nil {
		return nil, err
	}
	return &tikvrpc.CopStreamResponse{
		Tikv_CoprocessorStreamClient: new(mockCopStreamClient),
		Response:                     copResp,
	}, nil
}

// handleEstablishMPPConnection handle the mock mpp collection came from root or peers.
func (c *RPCClient) handleEstablishMPPConnection(ctx context.Context, r *mpp.EstablishMPPConnectionRequest, timeout time.Duration, storeID uint64) (*tikvrpc.MPPStreamResponse, error) {
	mockServer := new(mockMPPConnectStreamServer)
	err := c.usSvr.EstablishMPPConnectionWithStoreID(r, mockServer, storeID)
	if err != nil {
		return nil, err
	}
	failpoint.Inject("establishMppConnectionErr", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(nil, errors.New("rpc error"))
		}
	})
	var mockClient = mockMPPConnectionClient{mppResponses: mockServer.mppResponses, idx: 0, ctx: ctx, targetTask: r.ReceiverMeta}
	streamResp := &tikvrpc.MPPStreamResponse{Tikv_EstablishMPPConnectionClient: &mockClient}
	_, cancel := context.WithCancel(ctx)
	streamResp.Lease.Cancel = cancel
	streamResp.Timeout = timeout
	// mock the stream resp from the server's resp slice
	first, err := streamResp.Recv()
	if err != nil {
		if errors.Cause(err) != io.EOF {
			return nil, errors.Trace(err)
		}
	}
	streamResp.MPPDataPacket = first
	return streamResp, nil
}

func (c *RPCClient) handleDispatchMPPTask(ctx context.Context, r *mpp.DispatchTaskRequest, storeID uint64) (*mpp.DispatchTaskResponse, error) {
	return c.usSvr.DispatchMPPTaskWithStoreID(ctx, r, storeID)
}

func (c *RPCClient) handleBatchCop(ctx context.Context, r *coprocessor.BatchRequest, timeout time.Duration) (*tikvrpc.BatchCopStreamResponse, error) {
	mockBatchCopServer := &mockBatchCoprocessorStreamServer{}
	err := c.usSvr.BatchCoprocessor(r, mockBatchCopServer)
	if err != nil {
		return nil, err
	}
	var mockBatchCopClient = mockBatchCopClient{batchResponses: mockBatchCopServer.batchResponses, idx: 0}
	batchResp := &tikvrpc.BatchCopStreamResponse{Tikv_BatchCoprocessorClient: &mockBatchCopClient}
	_, cancel := context.WithCancel(ctx)
	batchResp.Lease.Cancel = cancel
	batchResp.Timeout = timeout
	first, err := batchResp.Recv()
	if err != nil {
		return nil, errors.Trace(err)
	}
	batchResp.BatchResponse = first
	return batchResp, nil
}

func (c *RPCClient) handleDebugGetRegionProperties(ctx context.Context, req *debugpb.GetRegionPropertiesRequest) (*debugpb.GetRegionPropertiesResponse, error) {
	region := c.cluster.GetRegion(req.RegionId)
	_, start, err := codec.DecodeBytes(region.StartKey, nil)
	if err != nil {
		return nil, err
	}
	_, end, err := codec.DecodeBytes(region.EndKey, nil)
	if err != nil {
		return nil, err
	}
	scanResp, err := c.usSvr.KvScan(ctx, &kvrpcpb.ScanRequest{
		Context: &kvrpcpb.Context{
			RegionId:    region.Id,
			RegionEpoch: region.RegionEpoch,
		},
		StartKey: start,
		EndKey:   end,
		Version:  math.MaxUint64,
		Limit:    math.MaxUint32,
	})
	if err != nil {
		return nil, err
	}
	if err := scanResp.GetRegionError(); err != nil {
		panic(err)
	}
	return &debugpb.GetRegionPropertiesResponse{
		Props: []*debugpb.Property{{
			Name:  "mvcc.num_rows",
			Value: strconv.Itoa(len(scanResp.Pairs)),
		}}}, nil
}

// Close closes RPCClient and cleanup temporal resources.
func (c *RPCClient) Close() error {
	atomic.StoreInt32(&c.closed, 1)
	if c.usSvr != nil {
		c.usSvr.Stop()
	}
	if !c.persistent && c.path != "" {
		err := os.RemoveAll(c.path)
		_ = err
	}
	return nil
}

// CloseAddr implements tikv.Client interface and it does nothing.
func (c *RPCClient) CloseAddr(addr string) error {
	return nil
}

// SetEventListener implements tikv.Client interface.
func (c *RPCClient) SetEventListener(listener tikv.ClientEventListener) {}

type mockClientStream struct{}

// Header implements grpc.ClientStream interface
func (mockClientStream) Header() (metadata.MD, error) { return nil, nil }

// Trailer implements grpc.ClientStream interface
func (mockClientStream) Trailer() metadata.MD { return nil }

// CloseSend implements grpc.ClientStream interface
func (mockClientStream) CloseSend() error { return nil }

// Context implements grpc.ClientStream interface
func (mockClientStream) Context() context.Context { return nil }

// SendMsg implements grpc.ClientStream interface
func (mockClientStream) SendMsg(m any) error { return nil }

// RecvMsg implements grpc.ClientStream interface
func (mockClientStream) RecvMsg(m any) error { return nil }

type mockCopStreamClient struct {
	mockClientStream
}

func (mock *mockCopStreamClient) Recv() (*coprocessor.Response, error) {
	return nil, io.EOF
}

type mockBatchCopClient struct {
	mockClientStream
	batchResponses []*coprocessor.BatchResponse
	idx            int
}

func (mock *mockBatchCopClient) Recv() (*coprocessor.BatchResponse, error) {
	if mock.idx < len(mock.batchResponses) {
		ret := mock.batchResponses[mock.idx]
		mock.idx++
		var err error
		if len(ret.OtherError) > 0 {
			err = errors.New(ret.OtherError)
			ret = nil
		}
		return ret, err
	}
	failpoint.Inject("batchCopRecvTimeout", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(nil, context.Canceled)
		}
	})
	return nil, io.EOF
}

type mockMPPConnectionClient struct {
	mockClientStream
	mppResponses []*mpp.MPPDataPacket
	idx          int
	ctx          context.Context
	targetTask   *mpp.TaskMeta
}

func (mock *mockMPPConnectionClient) Recv() (*mpp.MPPDataPacket, error) {
	if mock.idx < len(mock.mppResponses) {
		ret := mock.mppResponses[mock.idx]
		mock.idx++
		return ret, nil
	}
	failpoint.Inject("mppRecvTimeout", func(val failpoint.Value) {
		if int64(val.(int)) == mock.targetTask.TaskId {
			failpoint.Return(nil, context.Canceled)
		}
	})
	failpoint.Inject("mppRecvHang", func(val failpoint.Value) {
		for val.(bool) {
			select {
			case <-mock.ctx.Done():
				{
					failpoint.Return(nil, context.Canceled)
				}
			default:
				time.Sleep(1 * time.Second)
			}
		}
	})
	return nil, io.EOF
}

type mockServerStream struct{}

func (mockServerStream) SetHeader(metadata.MD) error  { return nil }
func (mockServerStream) SendHeader(metadata.MD) error { return nil }
func (mockServerStream) SetTrailer(metadata.MD)       {}
func (mockServerStream) Context() context.Context     { return nil }
func (mockServerStream) SendMsg(any) error            { return nil }
func (mockServerStream) RecvMsg(any) error            { return nil }

type mockBatchCoprocessorStreamServer struct {
	mockServerStream
	batchResponses []*coprocessor.BatchResponse
}

func (mockBatchCopServer *mockBatchCoprocessorStreamServer) Send(response *coprocessor.BatchResponse) error {
	mockBatchCopServer.batchResponses = append(mockBatchCopServer.batchResponses, response)
	return nil
}

type mockMPPConnectStreamServer struct {
	mockServerStream
	mppResponses []*mpp.MPPDataPacket
}

func (mockMPPConnectStreamServer *mockMPPConnectStreamServer) Send(mppResponse *mpp.MPPDataPacket) error {
	mockMPPConnectStreamServer.mppResponses = append(mockMPPConnectStreamServer.mppResponses, mppResponse)
	return nil
}
