package kgo

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kbin"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl"
)

type promisedReq struct {
	ctx     context.Context
	req     kmsg.Request
	promise func(kmsg.Response, error)
	enqueue time.Time // used to calculate writeWait
}

type promisedResp struct {
	ctx context.Context

	corrID int32
	// With flexible headers, we skip tags at the end of the response
	// header for now because they're currently unused. However, the
	// ApiVersions response uses v0 response header (no tags) even if the
	// response body has flexible versions. This is done in support of the
	// v0 fallback logic that allows for indexing into an exact offset.
	// Thus, for ApiVersions specifically, this is false even if the
	// request is flexible.
	//
	// As a side note, this note was not mentioned in KIP-482 which
	// introduced flexible versions, and was mentioned in passing in
	// KIP-511 which made ApiVersion flexible, so discovering what was
	// wrong was not too fun ("Note that ApiVersionsResponse is flexible
	// version but the response header is not flexible" is *it* in the
	// entire KIP.)
	//
	// To see the version pinning, look at the code generator function
	// generateHeaderVersion in
	// generator/src/main/java/org/apache/kafka/message/ApiMessageTypeGenerator.java
	flexibleHeader bool

	resp        kmsg.Response
	promise     func(kmsg.Response, error)
	readTimeout time.Duration

	// The following block is used for the read / e2e hooks.
	bytesWritten int
	writeWait    time.Duration
	timeToWrite  time.Duration
	readEnqueue  time.Time
}

func logID(id int32) string {
	if id >= -10 {
		return strconv.FormatInt(int64(id), 10)
	}
	return "seed " + strconv.FormatInt(int64(id)-math.MinInt32, 10)
}

// BrokerMetadata is metadata for a broker.
//
// This struct mirrors kmsg.MetadataResponseBroker.
type BrokerMetadata struct {
	// NodeID is the broker node ID.
	//
	// Seed brokers will have very negative IDs; kgo does not try to map
	// seed brokers to loaded brokers.
	NodeID int32

	// Port is the port of the broker.
	Port int32

	// Host is the hostname of the broker.
	Host string

	// Rack is an optional rack of the broker. It is invalid to modify this
	// field.
	//
	// Seed brokers will not have a rack.
	Rack *string

	_ struct{} // allow us to add fields later
}

func (me BrokerMetadata) equals(other kmsg.MetadataResponseBroker) bool {
	return me.NodeID == other.NodeID &&
		me.Port == other.Port &&
		me.Host == other.Host &&
		(me.Rack == nil && other.Rack == nil ||
			me.Rack != nil && other.Rack != nil && *me.Rack == *other.Rack)
}

// broker manages the concept how a client would interact with a broker.
type broker struct {
	cl *Client

	addr string // net.JoinHostPort(meta.Host, meta.Port)
	meta BrokerMetadata

	// versions tracks the first load of an ApiVersions. We store this
	// after the first connect, which helps speed things up on future
	// reconnects (across any of the three broker connections) because we
	// will never look up API versions for this broker again.
	versions atomic.Value // *brokerVersions

	// The cxn fields each manage a single tcp connection to one broker.
	// Each field is managed serially in handleReqs. This means that only
	// one write can happen at a time, regardless of which connection the
	// write goes to, but the write is expected to be fast whereas the wait
	// for the response is expected to be slow.
	//
	// Produce requests go to cxnProduce, fetch to cxnFetch, join/sync go
	// to cxnGroup, anything with TimeoutMillis goes to cxnSlow, and
	// everything else goes to cxnNormal.
	cxnNormal  *brokerCxn
	cxnProduce *brokerCxn
	cxnFetch   *brokerCxn
	cxnGroup   *brokerCxn
	cxnSlow    *brokerCxn

	reapMu sync.Mutex // held when modifying a brokerCxn

	// reqs manages incoming message requests.
	reqs ringReq
	// dead is an atomic so a backed up reqs cannot block broker stoppage.
	dead int32
}

// brokerVersions is loaded once (and potentially a few times concurrently if
// multiple connections are opening at once) and then forever stored for a
// broker.
type brokerVersions struct {
	versions [kmsg.MaxKey + 1]int16
}

func newBrokerVersions() *brokerVersions {
	var v brokerVersions
	for i := range &v.versions {
		v.versions[i] = -1
	}
	return &v
}

func (*brokerVersions) len() int { return kmsg.MaxKey + 1 }

func (b *broker) loadVersions() *brokerVersions {
	loaded := b.versions.Load()
	if loaded == nil {
		return nil
	}
	return loaded.(*brokerVersions)
}

func (b *broker) storeVersions(v *brokerVersions) { b.versions.Store(v) }

const unknownControllerID = -1

var unknownBrokerMetadata = BrokerMetadata{
	NodeID: -1,
}

// broker IDs are all positive, but Kafka uses -1 to signify unknown
// controllers. To avoid issues where a client broker ID map knows of
// a -1 ID controller, we start unknown seeds at MinInt32.
func unknownSeedID(seedNum int) int32 {
	return int32(math.MinInt32 + seedNum)
}

func (cl *Client) newBroker(nodeID int32, host string, port int32, rack *string) *broker {
	return &broker{
		cl: cl,

		addr: net.JoinHostPort(host, strconv.Itoa(int(port))),
		meta: BrokerMetadata{
			NodeID: nodeID,
			Host:   host,
			Port:   port,
			Rack:   rack,
		},
	}
}

// stopForever permanently disables this broker.
func (b *broker) stopForever() {
	if atomic.SwapInt32(&b.dead, 1) == 1 {
		return
	}

	b.reqs.die() // no more pushing

	b.reapMu.Lock()
	defer b.reapMu.Unlock()

	b.cxnNormal.die()
	b.cxnProduce.die()
	b.cxnFetch.die()
	b.cxnGroup.die()
	b.cxnSlow.die()
	requestId := "empty-string-id"
	if realId, ok := b.cl.ctx.Value("requestId").(string); ok {
		requestId = realId
	}
	b.cl.cfg.logger.Log(LogLevelDebug, "xing-stopping all brokers", "requestId", requestId)
}

// do issues a request to the broker, eventually calling the response
// once a the request either fails or is responded to (with failure or not).
//
// The promise will block broker processing.
func (b *broker) do(
	ctx context.Context,
	req kmsg.Request,
	promise func(kmsg.Response, error),
) {
	pr := promisedReq{ctx, req, promise, time.Now()}

	first, dead := b.reqs.push(pr)

	if first {
		go b.handleReqs(pr)
	} else if dead {
		promise(nil, errChosenBrokerDead)
	}
}

// waitResp runs a req, waits for the resp and returns the resp and err.
func (b *broker) waitResp(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	var resp kmsg.Response
	var err error
	done := make(chan struct{})
	wait := func(kresp kmsg.Response, kerr error) {
		resp, err = kresp, kerr
		close(done)
	}
	b.do(ctx, req, wait)
	<-done
	return resp, err
}

func (b *broker) handleReqs(pr promisedReq) {
	var more, dead bool
start:
	if dead {
		pr.promise(nil, errChosenBrokerDead)
	} else {
		b.handleReq(pr)
	}

	pr, more, dead = b.reqs.dropPeek()
	if more {
		goto start
	}
}

func (b *broker) handleReq(pr promisedReq) {
	req := pr.req
	var requestId string
	if _, ok := pr.ctx.Value("requestId").(string); ok {
		requestId = pr.ctx.Value("requestId").(string)
	} else {
		requestId = "empty-request-id"
	}
	restartAttempt := 0
restart:
	if restartAttempt > 0 {
		b.cl.cfg.logger.Log(LogLevelDebug, "xing-load-connection-with-retries", "restartAttempt", restartAttempt, "requestId", requestId)
	}
	var cxn *brokerCxn
	{
		var err error
		if cxn, err = b.loadConnection(requestId, pr.ctx, req); err != nil {
			b.cl.cfg.logger.Log(LogLevelDebug, "xing-loadConnections-Error", "err", err, "requestId", requestId)
			//if strings.Contains(err.Error(), "write: broken pipe") && restartAttempt < 3 {
			//	restartAttempt++
			//	b.cl.cfg.logger.Log(LogLevelDebug, "xing-retry-after-loadConnections", "restartAttempt", restartAttempt, "requestId", requestId, "err", err)
			//	time.Sleep(100 * time.Millisecond)
			//	goto restart
			//}
			pr.promise(nil, err)
			return
		}
	}
	b.cl.cfg.logger.Log(LogLevelDebug, "xing-loadConnections-success", "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
	v := b.loadVersions()

	if int(req.Key()) > v.len() || b.cl.cfg.maxVersions != nil && !b.cl.cfg.maxVersions.HasKey(req.Key()) {
		pr.promise(nil, errUnknownRequestKey)
		return
	}

	// If v.versions[0] is non-negative, then we loaded API
	// versions. If the version for this request is negative, we
	// know the broker cannot handle this request.
	if v.versions[0] >= 0 && v.versions[req.Key()] < 0 {
		pr.promise(nil, errBrokerTooOld)
		return
	}

	ourMax := req.MaxVersion()
	if b.cl.cfg.maxVersions != nil {
		userMax, _ := b.cl.cfg.maxVersions.LookupMaxKeyVersion(req.Key()) // we validated HasKey above
		if userMax < ourMax {
			ourMax = userMax
		}
	}

	// If brokerMax is negative at this point, we have no api
	// versions because the client is pinned pre 0.10.0 and we
	// stick with our max.
	version := ourMax
	if brokerMax := v.versions[req.Key()]; brokerMax >= 0 && brokerMax < ourMax {
		version = brokerMax
	}

	// If the version now (after potential broker downgrading) is
	// lower than we desire, we fail the request for the broker is
	// too old.
	if b.cl.cfg.minVersions != nil {
		minVersion, minVersionExists := b.cl.cfg.minVersions.LookupMaxKeyVersion(req.Key())
		if minVersionExists && version < minVersion {
			pr.promise(nil, errBrokerTooOld)
			return
		}
	}

	req.SetVersion(version) // always go for highest version

	now := time.Now()
	b.cl.cfg.logger.Log(LogLevelDebug, "xing-expiry time for cxn", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "expiry", cxn.expiry.UTC().String(), "now at", now, "is after", now.After(cxn.expiry), "authRequestId", cxn.AuthRequestId)
	for reauthentications := 1; !cxn.expiry.IsZero() && time.Now().After(cxn.expiry); reauthentications++ {
		// We allow 15 reauths, which is a lot. If a new lifetime is
		// <2.5s, we sleep 100ms and try again. Retrying 15x puts us at
		// <1s compared to the original lifetime. A broker should not
		// reply with a <1s lifetime, but if we end up here, then we
		// kill the connection ourselves and retry on a new connection.
		if reauthentications > 15 {
			cxn.cl.cfg.logger.Log(LogLevelError, "the broker has repeatedly given us short sasl lifetimes, we are forcefully killing our own connection to retry on a new connection ", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "authRequestId", cxn.AuthRequestId)
			pr.promise(nil, errSaslReauthLoop)
			cxn.die()
			return
		}

		// If we are after the reauth time, try to reauth. We
		// can only have an expiry if we went the authenticate
		// flow, so we know we are authenticating again.
		// For KIP-368.
		b.cl.cfg.logger.Log(LogLevelDebug, "sasl expiry limit reached, reauthenticating", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId)
		if err := cxn.sasl(requestId); err != nil {
			b.cl.cfg.logger.Log(LogLevelDebug, "xing-error during sasl reauthenticating and killing connection", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "err", err, "broker", cxn.addr, "authRequestId", cxn.AuthRequestId)
			cxn.die() // connection is either closed by us at somewhere or by the server. In both cases, we need to close the current one and create a new conneciton.
			//if strings.Contains(err.Error(), "write: broken pipe") && restartAttempt < 3 {
			//	restartAttempt++
			//	b.cl.cfg.logger.Log(LogLevelDebug, "xing-retry-after-sasl", "restartAttempt", restartAttempt, "requestId", requestId, "err", err)
			//	time.Sleep(100 * time.Millisecond)
			//	goto restart
			//}
			pr.promise(nil, err)
			return
		}
	}

	// Juuuust before we issue the request, we check if it was
	// canceled. We could have previously tried this request, which
	// then failed and retried.
	//
	// Checking the context was canceled here ensures we do not
	// loop. We could be more precise with error tracking, though.
	select {
	case <-pr.ctx.Done():
		b.cl.cfg.logger.Log(LogLevelDebug, "xing-context-canceled-Error", "err", pr.ctx.Err(), "requestId", requestId)
		pr.promise(nil, pr.ctx.Err())
		return
	default:
	}

	// Produce requests (and only produce requests) can be written
	// without receiving a reply. If we see required acks is 0,
	// then we immediately call the promise with no response.
	//
	// We provide a non-nil *kmsg.ProduceResponse for
	// *kmsg.ProduceRequest just to ensure we do not return with no
	// error and no kmsg.Response, per the client contract.
	//
	// As documented on the client's Request function, if this is a
	// *kmsg.ProduceRequest, we rewrite the acks to match the
	// client configured acks, and we rewrite the timeout millis if
	// acks is 0. We do this to ensure that our discard goroutine
	// is used correctly, and so that we do not write a request
	// with 0 acks and then send it to handleResps where it will
	// not get a response.
	var isNoResp bool
	var noResp *kmsg.ProduceResponse
	switch r := req.(type) {
	case *produceRequest:
		isNoResp = r.acks == 0
	case *kmsg.ProduceRequest:
		r.Acks = b.cl.cfg.acks.val
		if r.Acks == 0 {
			isNoResp = true
			r.TimeoutMillis = int32(b.cl.cfg.produceTimeout.Milliseconds())
		}
		noResp = kmsg.NewPtrProduceResponse()
		noResp.Version = req.GetVersion()
	}

	corrID, bytesWritten, writeWait, timeToWrite, readEnqueue, writeErr := cxn.writeRequest(pr.ctx, pr.enqueue, req, requestId)

	if writeErr != nil {
		b.cl.cfg.logger.Log(LogLevelDebug, "xing-writeRequest-Error", "err", pr.ctx.Err(), "requestId", requestId)
		pr.promise(nil, writeErr)
		cxn.die()
		cxn.hookWriteE2E(req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
		return
	}

	if isNoResp {
		pr.promise(noResp, nil)
		cxn.hookWriteE2E(req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
		return
	}

	rt, _ := cxn.cl.connTimeouter.timeouts(req)

	cxn.waitResp(requestId, cxn.AuthRequestId, promisedResp{
		pr.ctx,
		corrID,
		req.IsFlexible() && req.Key() != 18, // response header not flexible if ApiVersions; see promisedResp doc
		req.ResponseKind(),
		pr.promise,
		rt,
		bytesWritten,
		writeWait,
		timeToWrite,
		readEnqueue,
	})

	if restartAttempt > 0 {
		b.cl.cfg.logger.Log(LogLevelDebug, "xing-retry-succeeded", "requestId", requestId)
	}
}

func (cxn *brokerCxn) hookWriteE2E(key int16, bytesWritten int, writeWait, timeToWrite time.Duration, writeErr error) {
	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(HookBrokerE2E); ok {
			h.OnBrokerE2E(cxn.b.meta, key, BrokerE2E{
				BytesWritten: bytesWritten,
				WriteWait:    writeWait,
				TimeToWrite:  timeToWrite,
				WriteErr:     writeErr,
			})
		}
	})
}

// bufPool is used to reuse issued-request buffers across writes to brokers.
type bufPool struct{ p *sync.Pool }

func newBufPool() bufPool {
	return bufPool{
		p: &sync.Pool{New: func() interface{} { r := make([]byte, 1<<10); return &r }},
	}
}

func (p bufPool) get() []byte  { return (*p.p.Get().(*[]byte))[:0] }
func (p bufPool) put(b []byte) { p.p.Put(&b) }

// loadConection returns the broker's connection, creating it if necessary
// and returning an error of if that fails.
func (b *broker) loadConnection(requestId string, ctx context.Context, req kmsg.Request) (*brokerCxn, error) {
	var (
		pcxn         = &b.cxnNormal
		isProduceCxn bool // see docs on brokerCxn.discard for why we do this
		reqKey       = req.Key()
		_, isTimeout = req.(kmsg.TimeoutRequest)
	)
	switch {
	case reqKey == 0:
		pcxn = &b.cxnProduce
		isProduceCxn = true
	case reqKey == 1:
		pcxn = &b.cxnFetch
	case reqKey == 11 || reqKey == 14: // join || sync
		pcxn = &b.cxnGroup
	case isTimeout:
		pcxn = &b.cxnSlow
	}

	if *pcxn != nil && atomic.LoadInt32(&(*pcxn).dead) == 0 {
		b.cl.cfg.logger.Log(LogLevelDebug, "xing-connection already exists", "addr", b.addr, "broker", logID(b.meta.NodeID), "requestId", requestId, "authRequestId", (*pcxn).AuthRequestId)
		return *pcxn, nil
	}

	conn, err := b.connect(requestId, ctx)
	if err != nil {
		return nil, err
	}

	cxn := &brokerCxn{
		cl: b.cl,
		b:  b,

		addr:   b.addr,
		conn:   conn,
		deadCh: make(chan struct{}),
	}
	if err = cxn.init(isProduceCxn, requestId); err != nil {
		b.cl.cfg.logger.Log(LogLevelDebug, "connection initialization failed", "addr", b.addr, "broker", logID(b.meta.NodeID), "err", err, "requestId", requestId)
		cxn.closeConn()
		return nil, err
	}
	b.cl.cfg.logger.Log(LogLevelDebug, "connection initialized successfully", "addr", b.addr, "broker", logID(b.meta.NodeID), "requestId", requestId)

	b.reapMu.Lock()
	defer b.reapMu.Unlock()
	*pcxn = cxn
	return cxn, nil
}

func (cl *Client) reapConnectionsLoop() {
	idleTimeout := cl.cfg.connIdleTimeout
	if idleTimeout < 0 { // impossible due to cfg.validate, but just in case
		return
	}

	ticker := time.NewTicker(idleTimeout)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-cl.ctx.Done():
			return
		case tick := <-ticker.C:
			start := time.Now()
			reaped := cl.reapConnections(idleTimeout)
			dur := time.Since(start)
			if reaped > 0 {
				cl.cfg.logger.Log(LogLevelDebug, "reaped connections",
					"time_since_last_reap", tick.Sub(last), "reap_dur", dur, "num_reaped", reaped)
			}
			last = tick
		}
	}
}

func (cl *Client) reapConnections(idleTimeout time.Duration) (total int) {
	cl.brokersMu.Lock()
	brokers := make([]*broker, 0, len(cl.brokers)+len(cl.seeds))
	brokers = append(brokers, cl.brokers...)
	brokers = append(brokers, cl.seeds...)
	cl.brokersMu.Unlock()

	for _, broker := range brokers {
		count := broker.reapConnections(idleTimeout)
		if count > 0 {
			cl.cfg.logger.Log(LogLevelDebug, "reaped connections", "broker", broker.addr, "count", count)
		}
		total += count
	}
	return total
}

func (b *broker) reapConnections(idleTimeout time.Duration) (total int) {
	b.reapMu.Lock()
	defer b.reapMu.Unlock()

	for _, cxn := range []*brokerCxn{
		b.cxnNormal,
		b.cxnProduce,
		b.cxnFetch,
		b.cxnGroup,
		b.cxnSlow,
	} {
		if cxn == nil || atomic.LoadInt32(&cxn.dead) == 1 {
			continue
		}

		// If we have not written nor read in a long time, the
		// connection can be reaped. If only one is idle, the other may
		// be busy (or may not happen):
		//
		// - produce can write but never read
		// - fetch can hang for a while reading (infrequent writes)

		lastWrite := time.Unix(0, atomic.LoadInt64(&cxn.lastWrite))
		lastRead := time.Unix(0, atomic.LoadInt64(&cxn.lastRead))

		writeIdle := time.Since(lastWrite) > idleTimeout && atomic.LoadUint32(&cxn.writing) == 0
		readIdle := time.Since(lastRead) > idleTimeout && atomic.LoadUint32(&cxn.reading) == 0

		if writeIdle && readIdle {
			cxn.die()
			total++
		}
	}
	return total
}

// connect connects to the broker's addr, returning the new connection.
func (b *broker) connect(requestId string, ctx context.Context) (net.Conn, error) {
	b.cl.cfg.logger.Log(LogLevelDebug, "opening connection to broker", "addr", b.addr, "broker", logID(b.meta.NodeID), "requestId", requestId)
	start := time.Now()
	conn, err := b.cl.cfg.dialFn(ctx, "tcp", b.addr)
	since := time.Since(start)
	b.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(HookBrokerConnect); ok {
			h.OnBrokerConnect(b.meta, since, conn, err)
		}
	})
	if err != nil {
		if !errors.Is(err, ErrClientClosed) && !strings.Contains(err.Error(), "operation was canceled") {
			b.cl.cfg.logger.Log(LogLevelWarn, "unable to open connection to broker", "addr", b.addr, "broker", logID(b.meta.NodeID), "err", err, "requestId", requestId)
		}
		return nil, fmt.Errorf("unable to dial: %w", err)
	}
	b.cl.cfg.logger.Log(LogLevelDebug, "connection opened to broker", "addr", b.addr, "broker", logID(b.meta.NodeID), "requestId", requestId)
	return conn, nil
}

// brokerCxn manages an actual connection to a Kafka broker. This is separate
// the broker struct to allow lazy connection (re)creation.
type brokerCxn struct {
	conn net.Conn

	cl *Client
	b  *broker

	addr string

	mechanism sasl.Mechanism
	expiry    time.Time

	throttleUntil int64 // atomic nanosec

	corrID int32

	// The following four fields are used for connection reaping.
	// Write is only updated in one location; read is updated in three
	// due to readConn, readConnAsync, and discard.
	lastWrite int64
	lastRead  int64
	writing   uint32
	reading   uint32

	successes uint64

	// resps manages reading kafka responses.
	resps ringResp
	// dead is an atomic so that a backed up resps cannot block cxn death.
	dead int32
	// closed in cloneConn; allows throttle waiting to quit
	deadCh        chan struct{}
	AuthRequestId string
}

func (cxn *brokerCxn) init(isProduceCxn bool, requestId string) error {
	cxn.cl.cfg.logger.Log(LogLevelDebug, "re-initializing connections", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId)
	hasVersions := cxn.b.loadVersions() != nil
	if !hasVersions {
		if cxn.b.cl.cfg.maxVersions == nil || cxn.b.cl.cfg.maxVersions.HasKey(18) {
			if err := cxn.requestAPIVersions(); err != nil {
				if !errors.Is(err, ErrClientClosed) {
					cxn.cl.cfg.logger.Log(LogLevelError, "unable to request api versions", "broker", logID(cxn.b.meta.NodeID), "err", err, "requestId", requestId)
				}
				return err
			}
		} else {
			// We have a max versions, and it indicates no support
			// for ApiVersions. We just store a default -1 set.
			cxn.b.storeVersions(newBrokerVersions())
		}
	}

	if err := cxn.sasl(requestId); err != nil {
		if !errors.Is(err, ErrClientClosed) {
			cxn.cl.cfg.logger.Log(LogLevelError, "unable to initialize sasl", "broker", logID(cxn.b.meta.NodeID), "err", err, "requestId", requestId)
		}
		return err
	}

	if isProduceCxn && cxn.cl.cfg.acks.val == 0 {
		go cxn.discard() // see docs on discard for why we do this
	}
	return nil
}

func (cxn *brokerCxn) requestAPIVersions() error {
	maxVersion := int16(3)

	// If the user configured a max versions, we check that the key exists
	// before entering this function. Thus, we expect exists to be true,
	// but we still doubly check it for sanity (as well as userMax, which
	// can only be non-negative based off of LookupMaxKeyVersion's API).
	if cxn.cl.cfg.maxVersions != nil {
		userMax, exists := cxn.cl.cfg.maxVersions.LookupMaxKeyVersion(18) // 18 == api versions
		if exists && userMax >= 0 {
			maxVersion = userMax
		}
	}

start:
	req := kmsg.NewPtrApiVersionsRequest()
	req.Version = maxVersion
	req.ClientSoftwareName = cxn.cl.cfg.softwareName
	req.ClientSoftwareVersion = cxn.cl.cfg.softwareVersion
	cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing api versions request", "broker", logID(cxn.b.meta.NodeID), "version", maxVersion)
	corrID, bytesWritten, writeWait, timeToWrite, readEnqueue, writeErr := cxn.writeRequest(nil, time.Now(), req, "requestAPIVersions")
	if writeErr != nil {
		cxn.hookWriteE2E(req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
		return writeErr
	}

	rt, _ := cxn.cl.connTimeouter.timeouts(req)
	// api versions does *not* use flexible response headers; see comment in promisedResp
	rawResp, err := cxn.readResponse(nil, req.Key(), req.GetVersion(), corrID, false, rt, bytesWritten, writeWait, timeToWrite, readEnqueue)
	if err != nil {
		return err
	}
	if len(rawResp) < 2 {
		return fmt.Errorf("invalid length %d short response from ApiVersions request", len(rawResp))
	}

	resp := req.ResponseKind().(*kmsg.ApiVersionsResponse)

	// If we used a version larger than Kafka supports, Kafka replies with
	// Version 0 and an UNSUPPORTED_VERSION error.
	//
	// Pre Kafka 2.4.0, we have to retry the request with version 0.
	// Post, Kafka replies with all versions.
	if rawResp[1] == 35 {
		if maxVersion == 0 {
			return errors.New("Kafka replied with UNSUPPORTED_VERSION to an ApiVersions request of version 0")
		}
		srawResp := string(rawResp)
		if srawResp == "\x00\x23\x00\x00\x00\x00" ||
			// EventHubs erroneously replies with v1, so we check
			// for that as well.
			srawResp == "\x00\x23\x00\x00\x00\x00\x00\x00\x00\x00" {
			cxn.cl.cfg.logger.Log(LogLevelDebug, "kafka does not know our ApiVersions version, downgrading to version 0 and retrying", "broker", logID(cxn.b.meta.NodeID))
			maxVersion = 0
			goto start
		}
		resp.Version = 0
	}

	if err = resp.ReadFrom(rawResp); err != nil {
		return fmt.Errorf("unable to read ApiVersions response: %w", err)
	}
	if len(resp.ApiKeys) == 0 {
		return errors.New("ApiVersions response invalidly contained no ApiKeys")
	}

	v := newBrokerVersions()
	for _, key := range resp.ApiKeys {
		if key.ApiKey > kmsg.MaxKey || key.ApiKey < 0 {
			continue
		}
		v.versions[key.ApiKey] = key.MaxVersion
	}
	cxn.b.storeVersions(v)
	return nil
}

func (cxn *brokerCxn) sasl(requestId string) error {
	if len(cxn.cl.cfg.sasls) == 0 {
		return nil
	}
	mechanism := cxn.cl.cfg.sasls[0]
	retried := false
	authenticate := false
	v := cxn.b.loadVersions()
	req := kmsg.NewPtrSASLHandshakeRequest()

start:
	if mechanism.Name() != "GSSAPI" && v.versions[req.Key()] >= 0 {
		req.Mechanism = mechanism.Name()
		req.Version = v.versions[req.Key()]
		cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing SASLHandshakeRequest", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId)
		corrID, bytesWritten, writeWait, timeToWrite, readEnqueue, writeErr := cxn.writeRequest(nil, time.Now(), req, requestId)
		if writeErr != nil {
			cxn.hookWriteE2E(req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
			cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-sasl-SASLHandshakeRequest-writeError", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "err", writeErr)
			return writeErr
		}

		rt, _ := cxn.cl.connTimeouter.timeouts(req)
		rawResp, err := cxn.readResponse(nil, req.Key(), req.GetVersion(), corrID, req.IsFlexible(), rt, bytesWritten, writeWait, timeToWrite, readEnqueue)
		if err != nil {
			cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-sasl-SASLHandshakeRequest-readResponseErr", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "err", err)
			return err
		}
		resp := req.ResponseKind().(*kmsg.SASLHandshakeResponse)
		if err = resp.ReadFrom(rawResp); err != nil {
			return err
		}

		err = kerr.ErrorForCode(resp.ErrorCode)
		if err != nil {
			if !retried && err == kerr.UnsupportedSaslMechanism {
				for _, ours := range cxn.cl.cfg.sasls[1:] {
					for _, supported := range resp.SupportedMechanisms {
						if supported == ours.Name() {
							mechanism = ours
							retried = true
							goto start
						}
					}
				}
			}
			cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-sasl-SASLHandshakeResponseeError", "broker", logID(cxn.b.meta.NodeID), "requestId", requestId, "err", err)
			return err
		}
		authenticate = req.Version == 1
	}
	cxn.cl.cfg.logger.Log(LogLevelDebug, "beginning sasl authentication", "broker", logID(cxn.b.meta.NodeID), "mechanism", mechanism.Name(), "authenticate", authenticate)
	cxn.mechanism = mechanism

	return cxn.doSasl(authenticate, requestId)
}

func (cxn *brokerCxn) doSasl(authenticate bool, requestId string) error {
	authCtx := context.WithValue(cxn.cl.ctx, "requestId", requestId)
	session, clientWrite, err := cxn.mechanism.Authenticate(authCtx, cxn.addr)
	if err != nil {
		cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl has early errors", "err", err, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
		return err
	}
	if len(clientWrite) == 0 {
		return fmt.Errorf("unexpected server-write sasl with mechanism %s", cxn.mechanism.Name())
	}
	cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl ", "session", session, "challenge", string(clientWrite), "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
	cxn.AuthRequestId = requestId
	prereq := time.Now() // used below for sasl lifetime calculation
	var lifetimeMillis int64

	// Even if we do not wrap our reads/writes in SASLAuthenticate, we
	// still use the SASLAuthenticate timeouts.
	rt, wt := cxn.cl.connTimeouter.timeouts(kmsg.NewPtrSASLAuthenticateRequest())

	// We continue writing until both the challenging is done AND the
	// responses are done. We can have an additional response once we
	// are done with challenges.
	step := -1
	for done := false; !done || len(clientWrite) > 0; {
		step++
		var challenge []byte

		if !authenticate {
			buf := cxn.cl.bufPool.get()

			buf = append(buf[:0], 0, 0, 0, 0)
			binary.BigEndian.PutUint32(buf, uint32(len(clientWrite)))
			buf = append(buf, clientWrite...)

			cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing raw sasl authenticate", "broker", logID(cxn.b.meta.NodeID), "step", step, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
			_, _, _, _, err = cxn.writeConn(context.Background(), buf, wt, time.Now())

			cxn.cl.bufPool.put(buf)

			if err != nil {
				cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl-writeConn error", "broker", logID(cxn.b.meta.NodeID), "step", step, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID), "err", err)
				return err
			}
			if !done {
				if _, challenge, _, _, err = cxn.readConn(context.Background(), rt, time.Now()); err != nil {
					cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl-readConn error", "broker", logID(cxn.b.meta.NodeID), "step", step, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID), "err", err)
					return err
				}
			}
		} else {
			req := kmsg.NewPtrSASLAuthenticateRequest()
			req.SASLAuthBytes = clientWrite
			req.Version = cxn.b.loadVersions().versions[req.Key()]
			cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing SASLAuthenticate", "broker", logID(cxn.b.meta.NodeID), "version", req.Version, "step", step, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))

			// Lifetime: we take the timestamp before we write our
			// request; see usage below for why.
			prereq = time.Now()
			corrID, bytesWritten, writeWait, timeToWrite, readEnqueue, writeErr := cxn.writeRequest(nil, time.Now(), req, requestId)

			// As mentioned above, we could have one final write
			// without reading a response back (kerberos). If this
			// is the case, we need to e2e.
			if writeErr != nil || done {
				cxn.hookWriteE2E(req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
				if writeErr != nil {
					cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl write error", "broker", logID(cxn.b.meta.NodeID), "version", req.Version, "step", step, "requestId", requestId, "err", writeErr)
					return writeErr
				}
			}
			if !done {
				rawResp, err := cxn.readResponse(nil, req.Key(), req.GetVersion(), corrID, req.IsFlexible(), rt, bytesWritten, writeWait, timeToWrite, readEnqueue)
				if err != nil {
					return err
				}
				resp := req.ResponseKind().(*kmsg.SASLAuthenticateResponse)
				if err = resp.ReadFrom(rawResp); err != nil {
					return err
				}

				if err = kerr.ErrorForCode(resp.ErrorCode); err != nil {
					cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl TAF error", "corrID", corrID, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
					if resp.ErrorMessage != nil {
						return fmt.Errorf("%s: %w", *resp.ErrorMessage, err)
					}
					return err
				}
				challenge = resp.SASLAuthBytes
				lifetimeMillis = resp.SessionLifetimeMillis
				if lifetimeMillis == 0 {
					cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-doSasl has values ",
						"lifetimeMillis", lifetimeMillis, "corrID", corrID, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID), "challenge", string(clientWrite))
				}
			}
		}

		clientWrite = nil

		if !done {
			if done, clientWrite, err = session.Challenge(challenge); err != nil {
				cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-sasl has errors", "error", err, "requestId", requestId, "broker", logID(cxn.b.meta.NodeID))
				return err
			}
		}
	}
	if lifetimeMillis > 0 {
		// Lifetime: we could have written our request instantaenously,
		// the broker calculating our session lifetime, and then the
		// broker / network hung for a bit when writing. We
		// pessimistically assume this worst case and take off the
		// final request e2e latency x1.1 from the lifetime.
		//
		// If the latency is <2.5s, we also pessimistically assume that
		// things may take 2.5s in the future.
		//
		// We may make our lifetime <0; brokers should use longer
		// lifetimes, but some do not in all cases. If our lifetime is
		// <100ms, we sleep for 100ms just to ensure we do not
		// spin-loop reauthenticating *too* much.
		latency := int64(float64(time.Since(prereq).Milliseconds()) * 1.1)
		if latency < 2500 {
			latency = 2500
		}

		useLifetime := lifetimeMillis - latency
		now := time.Now()
		cxn.expiry = now.Add(time.Duration(useLifetime) * time.Millisecond)
		cxn.cl.cfg.logger.Log(LogLevelDebug, "sasl has a limited lifetime",
			"broker", logID(cxn.b.meta.NodeID),
			"reauthenticate_in", cxn.expiry.Sub(now),
			"expiry", cxn.expiry,
			"requestId", requestId)
		if useLifetime < 0 {
			cxn.cl.cfg.logger.Log(LogLevelInfo, "sasl lifetime minus 2.5s lower bound latency results in immediate reauthentication, sleeping 100ms to avoid spin-loop",
				"broker", logID(cxn.b.meta.NodeID),
				"session_lifetime", time.Duration(lifetimeMillis)*time.Millisecond,
				"latency_lower_bound", time.Duration(latency)*time.Millisecond,
				"requestId", requestId,
			)
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// Some internal requests use the client context to issue requests, so if the
// client is closed, this select case can be selected. We want to return the
// proper error.
//
// This function is used in this file anywhere the client context can cause
// ErrClientClosed.
func maybeUpdateCtxErr(clientCtx, reqCtx context.Context, err *error) {
	if clientCtx == reqCtx {
		*err = ErrClientClosed
	}
}

// writeRequest writes a message request to the broker connection, bumping the
// connection's correlation ID as appropriate for the next write.
func (cxn *brokerCxn) writeRequest(ctx context.Context, enqueuedForWritingAt time.Time, req kmsg.Request, requestId string) (corrID int32, bytesWritten int, writeWait, timeToWrite time.Duration, readEnqueue time.Time, writeErr error) {
	// A nil ctx means we cannot be throttled.
	if ctx != nil {
		throttleUntil := time.Unix(0, atomic.LoadInt64(&cxn.throttleUntil))
		if sleep := time.Until(throttleUntil); sleep > 0 {
			after := time.NewTimer(sleep)
			select {
			case <-after.C:
			case <-ctx.Done():
				writeErr = ctx.Err()
				maybeUpdateCtxErr(cxn.cl.ctx, ctx, &writeErr)
			case <-cxn.cl.ctx.Done():
				writeErr = ErrClientClosed
			case <-cxn.deadCh:
				writeErr = errChosenBrokerDead
			}
			if writeErr != nil {
				after.Stop()
				writeWait = time.Since(enqueuedForWritingAt)
				cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-write error during throttling", "err", writeErr, "writeWait", writeWait, "requestId", requestId)
				return
			}
		}
	}

	buf := cxn.cl.reqFormatter.AppendRequest(
		cxn.cl.bufPool.get()[:0],
		req,
		cxn.corrID,
	)

	if writeErr != nil {
		cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-write error after throttling", "requestId", requestId)
	}

	_, wt := cxn.cl.connTimeouter.timeouts(req)
	bytesWritten, writeWait, timeToWrite, readEnqueue, writeErr = cxn.writeConn(ctx, buf, wt, enqueuedForWritingAt)

	if writeErr != nil {
		cxn.cl.cfg.logger.Log(LogLevelDebug, "xing-write error after write conn", "timeout", wt, "requestId", requestId)
	}

	cxn.cl.bufPool.put(buf)

	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(HookBrokerWrite); ok {
			h.OnBrokerWrite(cxn.b.meta, req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
		}
	})
	if logger := cxn.cl.cfg.logger; logger.Level() >= LogLevelDebug {
		logger.Log(LogLevelDebug, fmt.Sprintf("wrote %s v%d", kmsg.NameForKey(req.Key()), req.GetVersion()), "broker", logID(cxn.b.meta.NodeID), "bytes_written", bytesWritten, "write_wait", writeWait, "time_to_write", timeToWrite, "err", writeErr, "requestId", requestId)
	}

	if writeErr != nil {
		return
	}
	corrID = cxn.corrID
	cxn.corrID++
	return
}

func (cxn *brokerCxn) writeConn(
	ctx context.Context,
	buf []byte,
	timeout time.Duration,
	enqueuedForWritingAt time.Time,
) (bytesWritten int, writeWait, timeToWrite time.Duration, readEnqueue time.Time, writeErr error) {
	atomic.SwapUint32(&cxn.writing, 1)
	defer func() {
		atomic.StoreInt64(&cxn.lastWrite, time.Now().UnixNano())
		atomic.SwapUint32(&cxn.writing, 0)
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		cxn.conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	defer cxn.conn.SetWriteDeadline(time.Time{})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeStart := time.Now()
		bytesWritten, writeErr = cxn.conn.Write(buf)
		// As soon as we are done writing, we track that we have now
		// enqueued this request for reading.
		readEnqueue = time.Now()
		writeWait = writeStart.Sub(enqueuedForWritingAt)
		timeToWrite = readEnqueue.Sub(writeStart)
	}()
	select {
	case <-writeDone:
	case <-cxn.cl.ctx.Done():
		cxn.conn.SetWriteDeadline(time.Now())
		<-writeDone
		if writeErr != nil {
			writeErr = ErrClientClosed
		}
	case <-ctx.Done():
		cxn.conn.SetWriteDeadline(time.Now())
		<-writeDone
		if writeErr != nil && ctx.Err() != nil {
			writeErr = ctx.Err()
			maybeUpdateCtxErr(cxn.cl.ctx, ctx, &writeErr)
		}
	}
	return
}

func (cxn *brokerCxn) readConn(
	ctx context.Context,
	timeout time.Duration,
	enqueuedForReadingAt time.Time,
) (nread int, buf []byte, readWait, timeToRead time.Duration, err error) {
	atomic.SwapUint32(&cxn.reading, 1)
	defer func() {
		atomic.StoreInt64(&cxn.lastRead, time.Now().UnixNano())
		atomic.SwapUint32(&cxn.reading, 0)
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		cxn.conn.SetReadDeadline(time.Now().Add(timeout))
	}
	defer cxn.conn.SetReadDeadline(time.Time{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sizeBuf := make([]byte, 4)
		readStart := time.Now()
		defer func() {
			timeToRead = time.Since(readStart)
			readWait = readStart.Sub(enqueuedForReadingAt)
		}()
		if nread, err = io.ReadFull(cxn.conn, sizeBuf); err != nil {
			return
		}
		var size int32
		if size, err = cxn.parseReadSize(sizeBuf); err != nil {
			return
		}
		buf = make([]byte, size)
		var nread2 int
		nread2, err = io.ReadFull(cxn.conn, buf)
		nread += nread2
		buf = buf[:nread2]
		if err != nil {
			return
		}
	}()
	select {
	case <-readDone:
	case <-cxn.cl.ctx.Done():
		cxn.conn.SetReadDeadline(time.Now())
		<-readDone
		if err != nil {
			err = ErrClientClosed
		}
	case <-ctx.Done():
		cxn.conn.SetReadDeadline(time.Now())
		<-readDone
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
			maybeUpdateCtxErr(cxn.cl.ctx, ctx, &err)
		}
	}
	return
}

// Parses a length 4 slice and enforces the min / max read size based off the
// client configuration.
func (cxn *brokerCxn) parseReadSize(sizeBuf []byte) (int32, error) {
	size := int32(binary.BigEndian.Uint32(sizeBuf))
	if size < 0 {
		return 0, fmt.Errorf("invalid negative response size %d", size)
	}
	if maxSize := cxn.b.cl.cfg.maxBrokerReadBytes; size > maxSize {
		if maxSize == 0x48545450 { // "HTTP"
			return 0, fmt.Errorf("invalid large response size %d > limit %d; the four size bytes are 'HTTP' in ascii, the beginning of an HTTP response; is your broker port correct?", size, maxSize)
		}
		// A TLS alert is 21, and a TLS alert has the version
		// following, where all major versions are 03xx. We
		// look for an alert and major version byte to suspect
		// if this we received a TLS alert.
		tlsVersion := uint16(sizeBuf[1])<<8 | uint16(sizeBuf[2])
		if sizeBuf[0] == 21 && tlsVersion&0x0300 != 0 {
			versionGuess := fmt.Sprintf("unknown TLS version (hex %x)", tlsVersion)
			for _, guess := range []struct {
				num  uint16
				text string
			}{
				{tls.VersionSSL30, "SSL v3"},
				{tls.VersionTLS10, "TLS v1.0"},
				{tls.VersionTLS11, "TLS v1.1"},
				{tls.VersionTLS12, "TLS v1.2"},
				{tls.VersionTLS13, "TLS v1.3"},
			} {
				if tlsVersion == guess.num {
					versionGuess = guess.text
				}
			}
			return 0, fmt.Errorf("invalid large response size %d > limit %d; the first three bytes received appear to be a tls alert record for %s; is this a plaintext connection speaking to a tls endpoint?", size, maxSize, versionGuess)
		}
		return 0, fmt.Errorf("invalid large response size %d > limit %d", size, maxSize)
	}
	return size, nil
}

// readResponse reads a response from conn, ensures the correlation ID is
// correct, and returns a newly allocated slice on success.
//
// This takes a bunch of extra arguments in support of HookBrokerE2E, overall
// this function takes 11 bytes in arguments.
func (cxn *brokerCxn) readResponse(
	ctx context.Context,
	key int16,
	version int16,
	corrID int32,
	flexibleHeader bool,
	timeout time.Duration,
	bytesWritten int,
	writeWait time.Duration,
	timeToWrite time.Duration,
	readEnqueue time.Time,
) ([]byte, error) {
	bytesRead, buf, readWait, timeToRead, readErr := cxn.readConn(ctx, timeout, readEnqueue)

	cxn.cl.cfg.hooks.each(func(h Hook) {
		switch h := h.(type) {
		case HookBrokerRead:
			h.OnBrokerRead(cxn.b.meta, key, bytesRead, readWait, timeToRead, readErr)
		case HookBrokerE2E:
			h.OnBrokerE2E(cxn.b.meta, key, BrokerE2E{
				BytesWritten: bytesWritten,
				BytesRead:    bytesRead,
				WriteWait:    writeWait,
				TimeToWrite:  timeToWrite,
				ReadWait:     readWait,
				TimeToRead:   timeToRead,
				ReadErr:      readErr,
			})
		}
	})
	if logger := cxn.cl.cfg.logger; logger.Level() >= LogLevelDebug {
		logger.Log(LogLevelDebug, fmt.Sprintf("read %s v%d", kmsg.NameForKey(key), version), "broker", logID(cxn.b.meta.NodeID), "bytes_read", bytesRead, "read_wait", readWait, "time_to_read", timeToRead, "err", readErr)
	}

	if readErr != nil {
		return nil, readErr
	}
	if len(buf) < 4 {
		return nil, kbin.ErrNotEnoughData
	}
	gotID := int32(binary.BigEndian.Uint32(buf))
	if gotID != corrID {
		return nil, errCorrelationIDMismatch
	}
	// If the response header is flexible, we skip the tags at the end of
	// it. They are currently unused.
	if flexibleHeader {
		b := kbin.Reader{Src: buf[4:]}
		kmsg.SkipTags(&b)
		return b.Src, b.Complete()
	}
	return buf[4:], nil
}

// closeConn is the one place we close broker connections. This is always done
// in either die, which is called when handleResps returns, or if init fails,
// which means we did not succeed enough to start handleResps.
func (cxn *brokerCxn) closeConn() {
	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(HookBrokerDisconnect); ok {
			h.OnBrokerDisconnect(cxn.b.meta, cxn.conn)
		}
	})
	cxn.conn.Close()
	close(cxn.deadCh)
}

// die kills a broker connection (which could be dead already) and replies to
// all requests awaiting responses appropriately.
func (cxn *brokerCxn) die() {
	if cxn == nil || atomic.SwapInt32(&cxn.dead, 1) == 1 {
		return
	}
	cxn.closeConn()
	cxn.resps.die()
	cxn.b.cl.cfg.logger.Log(LogLevelDebug, "killed connection", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID))
}

// waitResp, called serially by a broker's handleReqs, manages handling a
// message requests's response.
func (cxn *brokerCxn) waitResp(requestId string, authRequestId string, pr promisedResp) {
	first, dead := cxn.resps.push(pr)
	if first {
		go cxn.handleResps(requestId, pr)
	} else if dead {
		pr.promise(nil, errChosenBrokerDead)
		cxn.hookWriteE2E(pr.resp.Key(), pr.bytesWritten, pr.writeWait, pr.timeToWrite, errChosenBrokerDead)
	}
}

// If acks are zero, then a real Kafka installation never replies to produce
// requests. Unfortunately, Microsoft EventHubs rolled their own implementation
// and _does_ reply to ack-0 produce requests. We need to process these
// responses, because otherwise kernel buffers will fill up, Microsoft will be
// unable to reply, and then they will stop taking our produce requests.
//
// Thus, we just simply discard everything.
//
// Since we still want to support hooks, we still read the size of a response
// and then read that entire size before calling a hook. There are a few
// differences:
//
// (1) we do not know what version we produced, so we cannot validate the read,
// we just have to trust that the size is valid (and the data follows
// correctly).
//
// (2) rather than creating a slice for the response, we discard the entire
// response into a reusable small slice. The small size is because produce
// responses are relatively small to begin with, so we expect only a few reads
// per response.
//
// (3) we have no time for when the read was enqueued, so we miss that in the
// hook.
//
// (4) we start the time-to-read duration *after* the size bytes are read,
// since we have no idea when a read actually should start, since we should not
// receive responses to begin with.
//
// (5) we set a read deadline *after* the size bytes are read, and only if the
// client has not yet closed.
func (cxn *brokerCxn) discard() {
	var firstTimeout bool
	defer func() {
		if !firstTimeout { // see below
			cxn.die()
		} else {
			cxn.b.cl.cfg.logger.Log(LogLevelDebug, "produce acks==0 discard goroutine exiting; this broker looks to correctly not reply to ack==0 produce requests", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID))
		}
	}()

	discardBuf := make([]byte, 256)
	for i := 0; ; i++ {
		var (
			nread      int
			err        error
			timeToRead time.Duration

			deadlineMu  sync.Mutex
			deadlineSet bool

			readDone = make(chan struct{})
		)

		// On all but the first request, we use no deadline. We could
		// be hanging reading while we wait for more produce requests.
		// We know we are talking to azure when i > 0 and we should not
		// quit this goroutine.
		//
		// However, on the *first* produce request, we know that we are
		// writing *right now*. We can deadline our read side with
		// ample overhead, and if this first read hits the deadline,
		// then we can quit this discard / read goroutine with no
		// problems.
		//
		// We choose 3x our timeouts:
		//   - first we cover the write, connTimeoutOverhead + produceTimeout
		//   - then we cover the read, connTimeoutOverhead
		//   - then we throw in another connTimeoutOverhead just to be sure
		//
		deadline := time.Time{}
		if i == 0 {
			deadline = time.Now().Add(3*cxn.cl.cfg.requestTimeoutOverhead + cxn.cl.cfg.produceTimeout)
		}
		cxn.conn.SetReadDeadline(deadline)

		go func() {
			defer close(readDone)
			if nread, err = io.ReadFull(cxn.conn, discardBuf[:4]); err != nil {
				if i == 0 && errors.Is(err, os.ErrDeadlineExceeded) {
					firstTimeout = true
				}
				return
			}
			deadlineMu.Lock()
			if !deadlineSet {
				cxn.conn.SetReadDeadline(time.Now().Add(cxn.cl.cfg.produceTimeout))
			}
			deadlineMu.Unlock()

			atomic.SwapUint32(&cxn.reading, 1)
			defer func() {
				atomic.StoreInt64(&cxn.lastRead, time.Now().UnixNano())
				atomic.SwapUint32(&cxn.reading, 0)
			}()

			readStart := time.Now()
			defer func() { timeToRead = time.Since(readStart) }()
			var size int32
			if size, err = cxn.parseReadSize(discardBuf[:4]); err != nil {
				return
			}

			var nread2 int
			for size > 0 && err == nil {
				discard := discardBuf
				if int(size) < len(discard) {
					discard = discard[:size]
				}
				nread2, err = cxn.conn.Read(discard)
				nread += nread2
				size -= int32(nread2) // nread2 max is 128
			}
		}()

		select {
		case <-readDone:
		case <-cxn.cl.ctx.Done():
			deadlineMu.Lock()
			deadlineSet = true
			deadlineMu.Unlock()
			cxn.conn.SetReadDeadline(time.Now())
			<-readDone
			return
		}

		cxn.cl.cfg.hooks.each(func(h Hook) {
			if h, ok := h.(HookBrokerRead); ok {
				h.OnBrokerRead(cxn.b.meta, 0, nread, 0, timeToRead, err)
			}
		})
		if err != nil {
			return
		}
	}
}

// handleResps serially handles all broker responses for an single connection.
func (cxn *brokerCxn) handleResps(requestId string, pr promisedResp) {
	var more, dead bool
start:
	if dead {
		pr.promise(nil, errChosenBrokerDead)
		cxn.hookWriteE2E(pr.resp.Key(), pr.bytesWritten, pr.writeWait, pr.timeToWrite, errChosenBrokerDead)
	} else {
		cxn.handleResp(requestId, pr)
	}

	pr, more, dead = cxn.resps.dropPeek()
	if more {
		goto start
	}
}

func (cxn *brokerCxn) handleResp(requestId string, pr promisedResp) {
	rawResp, err := cxn.readResponse(
		pr.ctx,
		pr.resp.Key(),
		pr.resp.GetVersion(),
		pr.corrID,
		pr.flexibleHeader,
		pr.readTimeout,
		pr.bytesWritten,
		pr.writeWait,
		pr.timeToWrite,
		pr.readEnqueue,
	)
	if err != nil {
		if !errors.Is(err, ErrClientClosed) && !errors.Is(err, context.Canceled) {
			if cxn.successes > 0 || len(cxn.b.cl.cfg.sasls) > 0 {
				cxn.b.cl.cfg.logger.Log(LogLevelDebug, "read from broker errored, killing connection", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID), "successful_reads", cxn.successes, "err", err, "requestId", requestId)
			} else {
				cxn.b.cl.cfg.logger.Log(LogLevelWarn, "read from broker errored, killing connection after 0 successful responses (is sasl missing?)", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID), "err", err, "requestId", requestId)
			}
		}
		cxn.b.cl.cfg.logger.Log(LogLevelWarn, "xing-read from broker errored, killing connection ", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID), "successful_reads", cxn.successes, "err", err, "requestId", requestId)
		pr.promise(nil, err)
		cxn.die()
		return
	}

	cxn.successes++
	readErr := pr.resp.ReadFrom(rawResp)

	// If we had no error, we read the response successfully.
	//
	// Any response that can cause throttling satisfies the
	// kmsg.ThrottleResponse interface. We check that here.
	if readErr == nil {
		if throttleResponse, ok := pr.resp.(kmsg.ThrottleResponse); ok {
			millis, throttlesAfterResp := throttleResponse.Throttle()
			if millis > 0 {
				if throttlesAfterResp {
					throttleUntil := time.Now().Add(time.Millisecond * time.Duration(millis)).UnixNano()
					if throttleUntil > cxn.throttleUntil {
						atomic.StoreInt64(&cxn.throttleUntil, throttleUntil)
					}
				}
				cxn.cl.cfg.hooks.each(func(h Hook) {
					if h, ok := h.(HookBrokerThrottle); ok {
						h.OnBrokerThrottle(cxn.b.meta, time.Duration(millis)*time.Millisecond, throttlesAfterResp)
					}
				})
			}
		}
	} else {
		cxn.b.cl.cfg.logger.Log(LogLevelDebug, "read error from broker errored", "addr", cxn.b.addr, "broker", logID(cxn.b.meta.NodeID), "successful_reads", cxn.successes, "err", err, "requestId", requestId)
	}

	pr.promise(pr.resp, readErr)
}
