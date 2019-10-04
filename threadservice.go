package threads

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"

	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	gostream "github.com/libp2p/go-libp2p-gostream"
	p2phttp "github.com/libp2p/go-libp2p-http"
	"github.com/libp2p/go-libp2p-pubsub"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/textileio/go-textile-core/crypto"
	"github.com/textileio/go-textile-core/thread"
	tserv "github.com/textileio/go-textile-core/threadservice"
	tstore "github.com/textileio/go-textile-core/threadstore"
	"github.com/textileio/go-textile-threads/cbor"
	tserver "github.com/textileio/go-textile-threads/threadserver"
	"github.com/textileio/go-textile-threads/util"
)

const (
	IPEL                     = "ipel"
	IPELCode                 = 406
	IPELVersion              = "0.0.1"
	IPELProtocol protocol.ID = "/" + IPEL + "/" + IPELVersion
)

var addrProtocol = ma.Protocol{
	Name:       IPEL,
	Code:       IPELCode,
	VCode:      ma.CodeToVarint(IPELCode),
	Size:       ma.LengthPrefixedVarSize,
	Transcoder: ma.TranscoderP2P,
}

func init() {
	if err := ma.AddProtocol(addrProtocol); err != nil {
		panic(err)
	}
}

type threadservice struct {
	host       host.Host
	listener   net.Listener
	server     *tserver.Threadserver
	client     *http.Client
	dagService format.DAGService
	pubsub     *pubsub.PubSub
	ctx        context.Context
	cancel     context.CancelFunc
	tstore.Threadstore
}

func NewThreadservice(ctx context.Context, h host.Host, ds format.DAGService, ts tstore.Threadstore) (tserv.Threadservice, error) {
	listener, err := gostream.Listen(h, IPELProtocol)
	if err != nil {
		return nil, err
	}

	tr := &http.Transport{}
	tr.RegisterProtocol(IPEL, p2phttp.NewTransport(h, p2phttp.ProtocolOption(IPELProtocol)))

	service := &threadservice{
		host:        h,
		listener:    listener,
		client:      &http.Client{Transport: tr},
		dagService:  ds,
		Threadstore: ts,
	}

	service.server = tserver.NewThreadserver(func() tserv.Threadservice {
		return service
	})
	service.server.Open(listener)

	service.ctx, service.cancel = context.WithCancel(ctx)
	service.pubsub, err = pubsub.NewGossipSub(service.ctx, service.host)
	if err != nil {
		return nil, err
	}
	// @todo: ts.pubsub.RegisterTopicValidator()

	return service, nil
}

func (ts *threadservice) Close() (err error) {
	var errs []error
	weakClose := func(name string, c interface{}) {
		if cl, ok := c.(io.Closer); ok {
			if err = cl.Close(); err != nil {
				errs = append(errs, fmt.Errorf("%s error: %s", name, err))
			}
		}
	}

	weakClose("server", ts.server)
	//weakClose("host", ts.host)
	weakClose("listener", ts.listener)
	weakClose("dagservice", ts.dagService)
	weakClose("threadstore", ts.Threadstore)

	if len(errs) > 0 {
		return fmt.Errorf("failed while closing threadservice; err(s): %q", errs)
	}
	return nil
}

func (ts *threadservice) Host() host.Host {
	return ts.host
}

func (ts *threadservice) DAGService() format.DAGService {
	return ts.dagService
}

func (ts *threadservice) Add(ctx context.Context, body format.Node, opts ...tserv.AddOption) (l peer.ID, n thread.Node, err error) {
	// Get or create a log for the new node
	settings := tserv.AddOptions(opts...)
	log, err := ts.getOrCreateOwnLog(settings.Thread)
	if err != nil {
		return
	}

	// Write a node locally
	coded, err := ts.createNode(ctx, body, log, settings)
	if err != nil {
		return
	}

	// Send log to known writers
	for _, i := range ts.ThreadInfo(settings.Thread).Logs {
		if i.String() == log.ID.String() {
			continue
		}
		for _, a := range ts.Addrs(settings.Thread, i) {
			err = ts.send(ctx, coded, settings.Thread, log.ID, a)
			if err != nil {
				return
			}
		}
	}

	// Send to additional addresses
	for _, a := range settings.Addrs {
		err = ts.send(ctx, coded, settings.Thread, log.ID, a)
		if err != nil {
			return
		}
	}

	return log.ID, coded, nil
}

func (ts *threadservice) Put(ctx context.Context, node thread.Node, opts ...tserv.PutOption) error {
	// Get or create a log for the new node
	settings := tserv.PutOptions(opts...)
	log, err := ts.getOrCreateLog(settings.Thread, settings.Log)
	if err != nil {
		return err
	}

	// Save the node locally
	// Note: These get methods will return cached nodes.
	block, err := node.GetBlock(ctx, ts.dagService)
	if err != nil {
		return err
	}
	event, ok := block.(*cbor.Event)
	if !ok {
		return fmt.Errorf("invalid event")
	}
	header, err := event.GetHeader(ctx, ts.dagService, nil)
	if err != nil {
		return err
	}
	body, err := event.GetBody(ctx, ts.dagService, nil)
	if err != nil {
		return err
	}
	err = ts.dagService.AddMany(ctx, []format.Node{node, event, header, body})
	if err != nil {
		return err
	}

	ts.SetHead(settings.Thread, log.ID, node.Cid())
	return nil
}

// if own log, return local values
// if not, call addresses
func (ts *threadservice) Pull(ctx context.Context, t thread.ID, l peer.ID, opts ...tserv.PullOption) ([]thread.Node, error) {
	log := ts.LogInfo(t, l)

	settings := tserv.PullOptions(opts...)
	if !settings.Offset.Defined() {
		if len(log.Heads) == 0 {
			return nil, nil
		}
		settings.Offset = log.Heads[0]
	}

	followKey, err := crypto.ParseDecryptionKey(log.FollowKey)
	if err != nil {
		return nil, err
	}

	var nodes []thread.Node
	for i := 0; i < settings.Limit; i++ {
		node, err := cbor.GetNode(ctx, ts.dagService, settings.Offset, followKey)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)

		settings.Offset = node.PrevID()
		if !settings.Offset.Defined() {
			break
		}
	}
	return nodes, nil
}

func (ts *threadservice) Logs(t thread.ID) []thread.LogInfo {
	logs := make([]thread.LogInfo, 0)
	for _, l := range ts.ThreadInfo(t).Logs {
		log := ts.LogInfo(t, l)
		logs = append(logs, log)
	}
	return logs
}

func (ts *threadservice) Delete(ctx context.Context, t thread.ID) error {
	panic("implement me")
}

func (ts *threadservice) createLog() (info thread.LogInfo, err error) {
	return util.CreateLog(ts.host.ID())
}

func (ts *threadservice) getOrCreateLog(t thread.ID, l peer.ID) (info thread.LogInfo, err error) {
	info = ts.LogInfo(t, l)
	if info.PubKey != nil {
		return
	}
	info, err = ts.createLog()
	if err != nil {
		return
	}
	err = ts.AddLog(t, info)
	return
}

func (ts *threadservice) getOrCreateOwnLog(t thread.ID) (info thread.LogInfo, err error) {
	for _, id := range ts.LogsWithKeys(t) {
		if ts.PrivKey(t, id) != nil {
			info = ts.LogInfo(t, id)
			return
		}
	}
	info, err = ts.createLog()
	if err != nil {
		return
	}
	err = ts.AddLog(t, info)
	return
}

func (ts *threadservice) createNode(ctx context.Context, body format.Node, log thread.LogInfo, settings *tserv.AddSettings) (thread.Node, error) {
	if settings.Key == nil {
		var err error
		settings.Key, err = crypto.ParseEncryptionKey(log.ReadKey)
		if err != nil {
			return nil, err
		}
	}
	event, err := cbor.NewEvent(ctx, ts.dagService, body, settings)
	if err != nil {
		return nil, err
	}

	prev := cid.Undef
	if len(log.Heads) != 0 {
		prev = log.Heads[0]
	}
	followKey, err := crypto.ParseEncryptionKey(log.FollowKey)
	if err != nil {
		return nil, err
	}
	node, err := cbor.NewNode(ctx, ts.dagService, event, prev, log.PrivKey, followKey)
	if err != nil {
		return nil, err
	}

	ts.SetHead(settings.Thread, log.ID, node.Cid())

	return node, nil
}

func (ts *threadservice) send(ctx context.Context, node thread.Node, t thread.ID, l peer.ID, addr ma.Multiaddr) error {
	p, err := addr.ValueForProtocol(ma.P_P2P)
	if err != nil {
		return err
	}
	uri := fmt.Sprintf("%s://%s/threads/v0/%s/%s", IPEL, p, t.String(), l.String())
	payload, err := cbor.Marshal(ctx, ts.dagService, node)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cbor")

	sk := ts.Host().Peerstore().PrivKey(ts.Host().ID())
	if sk == nil {
		return fmt.Errorf("could not find key for host")
	}
	pk, err := sk.GetPublic().Bytes()
	if err != nil {
		return err
	}
	req.Header.Set("X-Identity", base64.StdEncoding.EncodeToString(pk))
	sig, err := sk.Sign(payload)
	if err != nil {
		return err
	}
	req.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(sig))

	fk := ts.FollowKey(t, l)
	if fk == nil {
		return fmt.Errorf("could not find follow key")
	}
	req.Header.Set("X-FollowKey", base64.StdEncoding.EncodeToString(fk))

	res, err := ts.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusCreated:
		fk, err := base64.StdEncoding.DecodeString(res.Header.Get("X-FollowKey"))
		if err != nil {
			return err
		}
		followKey, err := crypto.ParseDecryptionKey(fk)
		if err != nil {
			return err
		}
		rnode, err := cbor.Unmarshal(body, followKey)
		if err != nil {
			return err
		}
		event, err := cbor.GetEventFromNode(ctx, ts.dagService, rnode)
		if err != nil {
			return err
		}

		rk, err := base64.StdEncoding.DecodeString(res.Header.Get("X-ReadKey"))
		if err != nil {
			return err
		}
		readKey, err := crypto.ParseDecryptionKey(rk)
		if err != nil {
			return err
		}
		body, err := event.GetBody(ctx, ts.dagService, readKey)
		if err != nil {
			return err
		}
		logs, _, err := cbor.DecodeInvite(body)
		if err != nil {
			return err
		}
		for _, log := range logs {
			err = ts.AddLog(t, log)
			if err != nil {
				return err
			}
		}
	case http.StatusNoContent:
	default:
		var msg map[string]string
		err = json.Unmarshal(body, &msg)
		if err != nil {
			return err
		}
		return fmt.Errorf(msg["error"])
	}
	return nil
}
