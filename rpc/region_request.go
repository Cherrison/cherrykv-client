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
// See the License for the specific language governing permissions and
// limitations under the License.

package rpc

import (
	"context"
	"time"

	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/cherrison/cherrykv-client/locate"
	"github.com/cherrison/cherrykv-client/metrics"
	"github.com/cherrison/cherrykv-client/retry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrBodyMissing response body is missing error
var ErrBodyMissing = errors.New("response body is missing")

// RegionRequestSender sends KV/Cop requests to tikv server. It handles network
// errors and some region errors internally.
//
// Typically, a KV/Cop request is bind to a region, all keys that are involved
// in the request should be located in the region.
// The sending process begins with looking for the address of leader store's
// address of the target region from cache, and the request is then sent to the
// destination tikv server over TCP connection.
// If region is updated, can be caused by leader transfer, region split, region
// merge, or region balance, tikv server may not able to process request and
// send back a RegionError.
// RegionRequestSender takes care of errors that does not relevant to region
// range, such as 'I/O timeout', 'NotLeader', and 'ServerIsBusy'. For other
// errors, since region range have changed, the request may need to split, so we
// simply return the error to caller.
type RegionRequestSender struct {
	regionCache *locate.RegionCache
	client      Client
	storeAddr   string
	rpcError    error
}

// NewRegionRequestSender creates a new sender.
func NewRegionRequestSender(regionCache *locate.RegionCache, client Client) *RegionRequestSender {
	return &RegionRequestSender{
		regionCache: regionCache,
		client:      client,
	}
}

// RPCError returns an error if an RPC error is encountered during request.
func (s *RegionRequestSender) RPCError() error {
	return s.rpcError
}

// SendReq sends a request to tikv server.
func (s *RegionRequestSender) SendReq(bo *retry.Backoffer, req *Request, regionID locate.RegionVerID, timeout time.Duration) (*Response, error) {

	// gofail: var tikvStoreSendReqResult string
	// switch tikvStoreSendReqResult {
	// case "timeout":
	// 	 return nil, errors.New("timeout")
	// case "GCNotLeader":
	// 	 if req.Type == CmdGC {
	//		 return &Response{
	//			 Type:   CmdGC,
	//			 GC: &kvrpcpb.GCResponse{RegionError: &errorpb.Error{NotLeader: &errorpb.NotLeader{}}},
	//		 }, nil
	//	 }
	// case "GCServerIsBusy":
	//	 if req.Type == CmdGC {
	//		 return &Response{
	//			 Type: CmdGC,
	//			 GC:   &kvrpcpb.GCResponse{RegionError: &errorpb.Error{ServerIsBusy: &errorpb.ServerIsBusy{}}},
	//		 }, nil
	//	 }
	// }

	for {
		ctx, err := s.regionCache.GetRPCContext(bo, regionID)
		if err != nil {
			return nil, err
		}
		if ctx == nil {
			// If the region is not found in cache, it must be out
			// of date and already be cleaned up. We can skip the
			// RPC by returning RegionError directly.

			// TODO: Change the returned error to something like "region missing in cache",
			// and handle this error like StaleEpoch, which means to re-split the request and retry.
			return GenRegionErrorResp(req, &errorpb.Error{EpochNotMatch: &errorpb.EpochNotMatch{}})
		}

		s.storeAddr = ctx.Addr
		resp, retry, err := s.sendReqToRegion(bo, ctx, req, timeout)
		if err != nil {
			return nil, err
		}
		if retry {
			continue
		}

		regionErr, err := resp.GetRegionError()
		if err != nil {
			return nil, err
		}
		if regionErr != nil {
			retry, err := s.onRegionError(bo, ctx, regionErr)
			if err != nil {
				return nil, err
			}
			if retry {
				continue
			}
		}
		return resp, nil
	}
}

func (s *RegionRequestSender) sendReqToRegion(bo *retry.Backoffer, ctx *locate.RPCContext, req *Request, timeout time.Duration) (resp *Response, retry bool, err error) {
	if e := SetContext(req, ctx.Meta, ctx.Peer); e != nil {
		return nil, false, err
	}
	resp, err = s.client.SendRequest(bo.GetContext(), ctx.Addr, req, timeout)
	if err != nil {
		s.rpcError = err
		if e := s.onSendFail(bo, ctx, err); e != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	return
}

func (s *RegionRequestSender) onSendFail(bo *retry.Backoffer, ctx *locate.RPCContext, err error) error {
	// If it failed because the context is cancelled by ourself, don't retry.
	if errors.Cause(err) == context.Canceled {
		return err
	}
	code := codes.Unknown
	if s, ok := status.FromError(errors.Cause(err)); ok {
		code = s.Code()
	}
	if code == codes.Canceled {
		select {
		case <-bo.GetContext().Done():
			return err
		default:
			// If we don't cancel, but the error code is Canceled, it must be from grpc remote.
			// This may happen when tikv is killed and exiting.
			// Backoff and retry in this case.
			log.Warn("receive a grpc cancel signal from remote:", err)
		}
	}

	s.regionCache.DropStoreOnSendRequestFail(ctx, err)

	// Retry on send request failure when it's not canceled.
	// When a store is not available, the leader of related region should be elected quickly.
	// TODO: the number of retry time should be limited:since region may be unavailable
	// when some unrecoverable disaster happened.
	return bo.Backoff(retry.BoTiKVRPC, errors.Errorf("send tikv request error: %v, ctx: %v, try next peer later", err, ctx))
}

func regionErrorToLabel(e *errorpb.Error) string {
	if e.GetNotLeader() != nil {
		return "not_leader"
	} else if e.GetRegionNotFound() != nil {
		return "region_not_found"
	} else if e.GetKeyNotInRegion() != nil {
		return "key_not_in_region"
	} else if e.GetEpochNotMatch() != nil {
		return "epoch_not_match"
	} else if e.GetServerIsBusy() != nil {
		return "server_is_busy"
	} else if e.GetStaleCommand() != nil {
		return "stale_command"
	} else if e.GetStoreNotMatch() != nil {
		return "store_not_match"
	}
	return "unknown"
}

func (s *RegionRequestSender) onRegionError(bo *retry.Backoffer, ctx *locate.RPCContext, regionErr *errorpb.Error) (retryable bool, err error) {
	metrics.RegionErrorCounter.WithLabelValues(regionErrorToLabel(regionErr)).Inc()
	if notLeader := regionErr.GetNotLeader(); notLeader != nil {
		// Retry if error is `NotLeader`.
		log.Debugf("tikv reports `NotLeader`: %s, ctx: %v, retry later", notLeader, ctx)
		s.regionCache.UpdateLeader(ctx.Region, notLeader.GetLeader().GetStoreId())

		var boType retry.BackoffType
		if notLeader.GetLeader() != nil {
			boType = retry.BoUpdateLeader
		} else {
			boType = retry.BoRegionMiss
		}

		if err = bo.Backoff(boType, errors.Errorf("not leader: %v, ctx: %v", notLeader, ctx)); err != nil {
			return false, err
		}

		return true, nil
	}

	if storeNotMatch := regionErr.GetStoreNotMatch(); storeNotMatch != nil {
		// store not match
		log.Warnf("tikv reports `StoreNotMatch`: %s, ctx: %v, retry later", storeNotMatch, ctx)
		s.regionCache.ClearStoreByID(ctx.GetStoreID())
		return true, nil
	}

	if epochNotMatch := regionErr.GetEpochNotMatch(); epochNotMatch != nil {
		log.Debugf("tikv reports `StaleEpoch`, ctx: %v, retry later", ctx)
		err = s.regionCache.OnRegionStale(ctx, epochNotMatch.CurrentRegions)
		return false, err
	}
	if regionErr.GetServerIsBusy() != nil {
		log.Warnf("tikv reports `ServerIsBusy`, reason: %s, ctx: %v, retry later", regionErr.GetServerIsBusy().GetReason(), ctx)
		err = bo.Backoff(retry.BoServerBusy, errors.Errorf("server is busy, ctx: %v", ctx))
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if regionErr.GetStaleCommand() != nil {
		log.Debugf("tikv reports `StaleCommand`, ctx: %v", ctx)
		return true, nil
	}
	if regionErr.GetRaftEntryTooLarge() != nil {
		log.Warnf("tikv reports `RaftEntryTooLarge`, ctx: %v", ctx)
		return false, errors.New(regionErr.String())
	}
	// For other errors, we only drop cache here.
	// Because caller may need to re-split the request.
	log.Debugf("tikv reports region error: %s, ctx: %v", regionErr, ctx)
	s.regionCache.DropRegion(ctx.Region)
	return false, nil
}
