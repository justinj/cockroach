// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package storage

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/raft/raftpb"
	"github.com/rubyist/circuitbreaker"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	// Outgoing messages are queued per-node on a channel of this size.
	//
	// TODO(peter): The normal send buffer size is larger than we would like. It
	// is a temporary patch for the issue discussed in #8630 where
	// Store.HandleRaftRequest can block applying a preemptive snapshot for a
	// long enough period of time that grpc flow control kicks in and messages
	// are dropped on the sending side.
	raftSendBufferSize = 10000

	// When no message has been queued for this duration, the corresponding
	// instance of processQueue will shut down.
	//
	// TODO(tamird): make culling of outbound streams more evented, so that we
	// need not rely on this timeout to shut things down.
	raftIdleTimeout = time.Minute
)

// RaftMessageResponseStream is the subset of the
// MultiRaft_RaftMessageServer interface that is needed for sending responses.
type RaftMessageResponseStream interface {
	Context() context.Context
	Send(*RaftMessageResponse) error
}

// SnapshotResponseStream is the subset of the
// MultiRaft_RaftSnapshotServer interface that is needed for sending responses.
type SnapshotResponseStream interface {
	Context() context.Context
	Send(*SnapshotResponse) error
	Recv() (*SnapshotRequest, error)
}

// RaftMessageHandler is the interface that must be implemented by
// arguments to RaftTransport.Listen.
type RaftMessageHandler interface {
	// HandleRaftRequest is called for each incoming Raft message. If it returns
	// an error it will be streamed back to the sender of the message as a
	// RaftMessageResponse. If the stream parameter is nil the request should be
	// processed synchronously. If the stream is non-nil the request can be
	// processed asynchronously and any error should be sent on the stream.
	HandleRaftRequest(ctx context.Context, req *RaftMessageRequest,
		respStream RaftMessageResponseStream) *roachpb.Error

	// HandleRaftResponse is called for each raft response. Note that
	// not all messages receive a response. An error is returned if and only if
	// the underlying Raft connection should be closed.
	HandleRaftResponse(context.Context, *RaftMessageResponse) error

	// HandleSnapshot is called for each new incoming snapshot stream, after
	// parsing the initial SnapshotRequest_Header on the stream.
	HandleSnapshot(header *SnapshotRequest_Header, respStream SnapshotResponseStream) error
}

// NodeAddressResolver is the function used by RaftTransport to map node IDs to
// network addresses.
type NodeAddressResolver func(roachpb.NodeID) (net.Addr, error)

// GossipAddressResolver is a thin wrapper around gossip's GetNodeIDAddress
// that allows its return value to be used as the net.Addr interface.
func GossipAddressResolver(gossip *gossip.Gossip) NodeAddressResolver {
	return func(nodeID roachpb.NodeID) (net.Addr, error) {
		return gossip.GetNodeIDAddress(nodeID)
	}
}

type raftTransportStats struct {
	nodeID        roachpb.NodeID
	queue         int
	queueMax      int32
	clientSent    int64
	clientRecv    int64
	clientDropped int64
	serverSent    int64
	serverRecv    int64
}

type raftTransportStatsSlice []*raftTransportStats

func (s raftTransportStatsSlice) Len() int           { return len(s) }
func (s raftTransportStatsSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s raftTransportStatsSlice) Less(i, j int) bool { return s[i].nodeID < s[j].nodeID }

// RaftTransport handles the rpc messages for raft.
//
// The raft transport is asynchronous with respect to the caller, and
// internally multiplexes outbound messages. Internally, each message is
// queued on a per-destination queue before being asynchronously delivered.
//
// Callers are required to construct a RaftSender before being able to
// dispatch messages, and must provide an error handler which will be invoked
// asynchronously in the event that the recipient of any message closes its
// inbound RPC stream. This callback is asynchronous with respect to the
// outbound message which caused the remote to hang up; all that is known is
// which remote hung up.
type RaftTransport struct {
	log.AmbientContext

	resolver   NodeAddressResolver
	rpcContext *rpc.Context

	mu struct {
		syncutil.Mutex
		queues   map[roachpb.NodeID]chan *RaftMessageRequest
		stats    map[roachpb.NodeID]*raftTransportStats
		breakers map[roachpb.NodeID]*circuit.Breaker
	}

	recvMu struct {
		syncutil.Mutex
		handlers map[roachpb.StoreID]RaftMessageHandler
	}
}

// NewDummyRaftTransport returns a dummy raft transport for use in tests which
// need a non-nil raft transport that need not function.
func NewDummyRaftTransport() *RaftTransport {
	return NewRaftTransport(log.AmbientContext{}, nil, nil, nil)
}

// NewRaftTransport creates a new RaftTransport.
func NewRaftTransport(
	ambient log.AmbientContext,
	resolver NodeAddressResolver,
	grpcServer *grpc.Server,
	rpcContext *rpc.Context,
) *RaftTransport {
	t := &RaftTransport{
		AmbientContext: ambient,
		resolver:       resolver,
		rpcContext:     rpcContext,
	}
	t.mu.queues = make(map[roachpb.NodeID]chan *RaftMessageRequest)
	t.mu.stats = make(map[roachpb.NodeID]*raftTransportStats)
	t.mu.breakers = make(map[roachpb.NodeID]*circuit.Breaker)

	t.recvMu.handlers = make(map[roachpb.StoreID]RaftMessageHandler)

	if grpcServer != nil {
		RegisterMultiRaftServer(grpcServer, t)
	}

	if t.rpcContext != nil && log.V(1) {
		t.rpcContext.Stopper.RunWorker(func() {
			ctx := t.AnnotateCtx(context.Background())
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			lastStats := make(map[roachpb.NodeID]raftTransportStats)
			lastTime := timeutil.Now()
			var stats raftTransportStatsSlice
			for {
				select {
				case <-ticker.C:
					t.mu.Lock()
					stats = stats[:0]
					for _, s := range t.mu.stats {
						stats = append(stats, s)
						s.queue = 0
					}
					for nodeID, ch := range t.mu.queues {
						t.mu.stats[nodeID].queue += len(ch)
					}
					t.mu.Unlock()

					now := timeutil.Now()
					elapsed := now.Sub(lastTime).Seconds()
					sort.Sort(stats)

					var buf bytes.Buffer
					// NB: The header is 80 characters which should display in a single
					// line on most terminals.
					fmt.Fprintf(&buf,
						"         qlen   qmax   qdropped client-sent client-recv server-sent server-recv\n")
					for _, s := range stats {
						last := lastStats[s.nodeID]
						cur := raftTransportStats{
							nodeID:        s.nodeID,
							queue:         s.queue,
							queueMax:      atomic.LoadInt32(&s.queueMax),
							clientDropped: atomic.LoadInt64(&s.clientDropped),
							clientSent:    atomic.LoadInt64(&s.clientSent),
							clientRecv:    atomic.LoadInt64(&s.clientRecv),
							serverSent:    atomic.LoadInt64(&s.serverSent),
							serverRecv:    atomic.LoadInt64(&s.serverRecv),
						}
						fmt.Fprintf(&buf, "  %3d: %6d %6d %10d %11.1f %11.1f %11.1f %11.1f\n",
							cur.nodeID, cur.queue, cur.queueMax, cur.clientDropped,
							float64(cur.clientSent-last.clientSent)/elapsed,
							float64(cur.clientRecv-last.clientRecv)/elapsed,
							float64(cur.serverSent-last.serverSent)/elapsed,
							float64(cur.serverRecv-last.serverRecv)/elapsed)
						lastStats[s.nodeID] = cur
					}
					lastTime = now
					log.Infof(ctx, "stats:\n%s", buf.String())
				case <-t.rpcContext.Stopper.ShouldStop():
					return
				}
			}
		})
	}

	return t
}

// handleRaftRequest proxies a request to the listening server interface.
func (t *RaftTransport) handleRaftRequest(
	ctx context.Context, req *RaftMessageRequest, respStream RaftMessageResponseStream,
) *roachpb.Error {
	t.recvMu.Lock()
	handler, ok := t.recvMu.handlers[req.ToReplica.StoreID]
	t.recvMu.Unlock()

	if !ok {
		log.Warningf(ctx, "unable to accept Raft message from %+v: no handler registered for %+v",
			req.FromReplica, req.ToReplica)
		return roachpb.NewError(roachpb.NewStoreNotFoundError(req.ToReplica.StoreID))
	}

	return handler.HandleRaftRequest(ctx, req, respStream)
}

// newRaftMessageResponse constructs a RaftMessageResponse from the
// given request and error.
func newRaftMessageResponse(req *RaftMessageRequest, pErr *roachpb.Error) *RaftMessageResponse {
	resp := &RaftMessageResponse{
		RangeID: req.RangeID,
		// From and To are reversed in the response.
		ToReplica:   req.FromReplica,
		FromReplica: req.ToReplica,
	}
	if pErr != nil {
		resp.Union.SetValue(pErr)
	}
	return resp
}

func (t *RaftTransport) getStats(nodeID roachpb.NodeID) *raftTransportStats {
	t.mu.Lock()
	stats, ok := t.mu.stats[nodeID]
	if !ok {
		stats = &raftTransportStats{nodeID: nodeID}
		t.mu.stats[nodeID] = stats
	}
	t.mu.Unlock()
	return stats
}

// RaftMessageBatch proxies the incoming requests to the listening server interface.
func (t *RaftTransport) RaftMessageBatch(stream MultiRaft_RaftMessageBatchServer) error {
	errCh := make(chan error, 1)

	// Node stopping error is caught below in the select.
	if err := t.rpcContext.Stopper.RunTask(func() {
		t.rpcContext.Stopper.RunWorker(func() {
			errCh <- func() error {
				var stats *raftTransportStats
				for {
					batch, err := stream.Recv()
					if err != nil {
						return err
					}
					if len(batch.Requests) == 0 {
						continue
					}

					if stats == nil {
						stats = t.getStats(batch.Requests[0].FromReplica.NodeID)
					}

					for i := range batch.Requests {
						req := &batch.Requests[i]
						atomic.AddInt64(&stats.serverRecv, 1)
						if pErr := t.handleRaftRequest(stream.Context(), req, stream); pErr != nil {
							atomic.AddInt64(&stats.serverSent, 1)
							if err := stream.Send(newRaftMessageResponse(req, pErr)); err != nil {
								return err
							}
						}
					}
				}
			}()
		})
	}); err != nil {
		return err
	}

	select {
	case err := <-errCh:
		return err
	case <-t.rpcContext.Stopper.ShouldQuiesce():
		return nil
	}
}

// RaftSnapshot handles incoming streaming snapshot requests.
func (t *RaftTransport) RaftSnapshot(stream MultiRaft_RaftSnapshotServer) error {
	errCh := make(chan error, 1)
	if err := t.rpcContext.Stopper.RunAsyncTask(stream.Context(), func(ctx context.Context) {
		errCh <- func() error {
			req, err := stream.Recv()
			if err != nil {
				return err
			}
			if req.Header == nil {
				return stream.Send(&SnapshotResponse{
					Status:  SnapshotResponse_ERROR,
					Message: "client error: no header in first snapshot request message"})
			}
			rmr := req.Header.RaftMessageRequest
			t.recvMu.Lock()
			handler, ok := t.recvMu.handlers[rmr.ToReplica.StoreID]
			t.recvMu.Unlock()
			if !ok {
				log.Warningf(ctx, "unable to accept Raft message from %+v: no handler registered for %+v",
					rmr.FromReplica, rmr.ToReplica)
				return roachpb.NewStoreNotFoundError(rmr.ToReplica.StoreID)
			}

			return handler.HandleSnapshot(req.Header, stream)
		}()
	}); err != nil {
		return err
	}
	select {
	case <-t.rpcContext.Stopper.ShouldStop():
		return nil
	case err := <-errCh:
		return err
	}
}

// Listen registers a raftMessageHandler to receive proxied messages.
func (t *RaftTransport) Listen(storeID roachpb.StoreID, handler RaftMessageHandler) {
	t.recvMu.Lock()
	t.recvMu.handlers[storeID] = handler
	t.recvMu.Unlock()
}

// Stop unregisters a raftMessageHandler.
func (t *RaftTransport) Stop(storeID roachpb.StoreID) {
	t.recvMu.Lock()
	delete(t.recvMu.handlers, storeID)
	t.recvMu.Unlock()
}

// GetCircuitBreaker returns the circuit breaker controlling
// connection attempts to the specified node.
func (t *RaftTransport) GetCircuitBreaker(nodeID roachpb.NodeID) *circuit.Breaker {
	t.mu.Lock()
	defer t.mu.Unlock()
	breaker, ok := t.mu.breakers[nodeID]
	if !ok {
		breaker = t.rpcContext.NewBreaker()
		t.mu.breakers[nodeID] = breaker
	}
	return breaker
}

// connectAndProcess connects to the node and then processes the
// provided channel containing a queue of raft messages until there is
// an unrecoverable error with the underlying connection. A circuit
// breaker is used to allow fast failures in SendAsync which will drop
// incoming raft messages and report unreachable status to the raft group.
func (t *RaftTransport) connectAndProcess(
	ctx context.Context,
	nodeID roachpb.NodeID,
	ch chan *RaftMessageRequest,
	stats *raftTransportStats,
) {
	breaker := t.GetCircuitBreaker(nodeID)
	successes := breaker.Successes()
	consecFailures := breaker.ConsecFailures()
	// NB: We don't check breaker.Ready() here or use breaker.Call() (which
	// internally checks breaker.Ready()) because we've already done so in
	// RaftTransport.SendAsync() and the nature of half-open breakers is that
	// they only allow breaker.Ready() to return true once per backoff period.
	if err := func() error {
		addr, err := t.resolver(nodeID)
		if err != nil {
			return err
		}
		conn, err := t.rpcContext.GRPCDial(addr.String(), grpc.WithBlock())
		if err != nil {
			return err
		}
		client := NewMultiRaftClient(conn)
		batchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stream, err := client.RaftMessageBatch(batchCtx)
		if err != nil {
			return err
		}
		if successes == 0 || consecFailures > 0 {
			log.Infof(ctx, "raft transport stream to node %d established", nodeID)
		}
		breaker.Success()
		return t.processQueue(nodeID, ch, stats, stream)
	}(); err != nil {
		if consecFailures == 0 {
			log.Warningf(ctx, "raft transport stream to node %d failed: %s", nodeID, err)
		}
		breaker.Fail()
	}
}

// processQueue opens a Raft client stream and sends messages from the
// designated queue (ch) via that stream, exiting when an error is received or
// when it idles out. All messages remaining in the queue at that point are
// lost and a new instance of processQueue will be started by the next message
// to be sent.
func (t *RaftTransport) processQueue(
	nodeID roachpb.NodeID,
	ch chan *RaftMessageRequest,
	stats *raftTransportStats,
	stream MultiRaft_RaftMessageBatchClient,
) error {
	errCh := make(chan error, 1)

	// Starting workers in a task prevents data races during shutdown.
	if err := t.rpcContext.Stopper.RunTask(func() {
		t.rpcContext.Stopper.RunWorker(func() {
			ctx := t.AnnotateCtx(context.Background())
			errCh <- func() error {
				for {
					resp, err := stream.Recv()
					if err != nil {
						return err
					}
					atomic.AddInt64(&stats.clientRecv, 1)
					t.recvMu.Lock()
					handler, ok := t.recvMu.handlers[resp.ToReplica.StoreID]
					t.recvMu.Unlock()
					if !ok {
						log.Warningf(ctx, "no handler found for store %s in response %s",
							resp.ToReplica.StoreID, resp)
						continue
					}
					if err := handler.HandleRaftResponse(stream.Context(), resp); err != nil {
						return err
					}
				}
			}()
		})
	}); err != nil {
		return err
	}

	var raftIdleTimer timeutil.Timer
	defer raftIdleTimer.Stop()
	batch := &RaftMessageRequestBatch{}
	for {
		raftIdleTimer.Reset(raftIdleTimeout)
		select {
		case <-t.rpcContext.Stopper.ShouldStop():
			return nil
		case <-raftIdleTimer.C:
			raftIdleTimer.Read = true
			return nil
		case err := <-errCh:
			return err
		case req := <-ch:
			batch.Requests = append(batch.Requests, *req)

			// Pull off as many queued requests as possible.
			//
			// TODO(peter): Think about limiting the size of the batch we send.
			for done := false; !done; {
				select {
				case req = <-ch:
					batch.Requests = append(batch.Requests, *req)
				default:
					done = true
				}
			}

			err := stream.Send(batch)
			batch.Requests = batch.Requests[:0]

			atomic.AddInt64(&stats.clientSent, 1)
			if err != nil {
				return err
			}
		}
	}
}

// SendAsync sends a message to the recipient specified in the request. It
// returns false if the outgoing queue is full and calls s.onError when the
// recipient closes the stream.
func (t *RaftTransport) SendAsync(req *RaftMessageRequest) bool {
	if req.RangeID == 0 && len(req.Heartbeats) == 0 && len(req.HeartbeatResps) == 0 {
		// Coalesced heartbeats are addressed to range 0; everything else
		// needs an explicit range ID.
		panic("only messages with coalesced heartbeats or heartbeat responses may be sent to range ID 0")
	}
	if req.Message.Type == raftpb.MsgSnap {
		panic("snapshots must be sent using SendSnapshot")
	}
	toNodeID := req.ToReplica.NodeID

	// First, check the circuit breaker for connections to the outgoing
	// node. We fail fast if the breaker is open to drop the raft
	// message and have the caller mark the raft group as unreachable.
	if !t.GetCircuitBreaker(toNodeID).Ready() {
		stats := t.getStats(toNodeID)
		atomic.AddInt64(&stats.clientDropped, 1)
		return false
	}

	t.mu.Lock()
	stats, ok := t.mu.stats[toNodeID]
	if !ok {
		stats = &raftTransportStats{nodeID: toNodeID}
		t.mu.stats[toNodeID] = stats
	}
	ch, ok := t.mu.queues[toNodeID]
	if !ok {
		ch = make(chan *RaftMessageRequest, raftSendBufferSize)
		t.mu.queues[toNodeID] = ch
	}
	t.mu.Unlock()

	if !ok {
		deleteQueue := func() {
			t.mu.Lock()
			delete(t.mu.queues, toNodeID)
			t.mu.Unlock()
		}
		// Starting workers in a task prevents data races during shutdown.
		if err := t.rpcContext.Stopper.RunTask(func() {
			t.rpcContext.Stopper.RunWorker(func() {
				ctx := t.AnnotateCtx(context.Background())
				t.connectAndProcess(ctx, toNodeID, ch, stats)
				deleteQueue()
			})
		}); err != nil {
			deleteQueue()
			return false
		}
	}

	select {
	case ch <- req:
		l := int32(len(ch))
		if v := atomic.LoadInt32(&stats.queueMax); v < l {
			atomic.CompareAndSwapInt32(&stats.queueMax, v, l)
		}
		return true
	default:
		atomic.AddInt64(&stats.clientDropped, 1)
		return false
	}
}

// SendSnapshot streams the given outgoing snapshot. The caller is responsible
// for closing the OutgoingSnapshot.
func (t *RaftTransport) SendSnapshot(
	ctx context.Context,
	storePool *StorePool,
	header SnapshotRequest_Header,
	snap *OutgoingSnapshot,
	newBatch func() engine.Batch,
	sent func(),
) error {
	var stream MultiRaft_RaftSnapshotClient
	nodeID := header.RaftMessageRequest.ToReplica.NodeID
	breaker := t.GetCircuitBreaker(nodeID)
	if err := breaker.Call(func() error {
		addr, err := t.resolver(nodeID)
		if err != nil {
			return err
		}
		conn, err := t.rpcContext.GRPCDial(addr.String(), grpc.WithBlock())
		if err != nil {
			return err
		}
		client := NewMultiRaftClient(conn)
		stream, err = client.RaftSnapshot(ctx)
		return err
	}, 0); err != nil {
		return err
	}
	// Note that we intentionally do not open the breaker if we encounter an
	// error while sending the snapshot. Snapshots are much less common than
	// regular Raft messages so if there is a real problem with the remote we'll
	// probably notice it really soon. Conversely, snapshots are a bit weird
	// (much larger than regular messages) so if there is an error here we don't
	// want to disrupt normal messages.
	defer func() {
		if err := stream.CloseSend(); err != nil {
			log.Warningf(ctx, "failed to close snapshot stream: %s", err)
		}
	}()
	return sendSnapshot(ctx, stream, storePool, header, snap, newBatch, sent)
}
