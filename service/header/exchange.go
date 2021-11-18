package header

import (
	"context"
	"fmt"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"

	tmbytes "github.com/tendermint/tendermint/libs/bytes"

	pb "github.com/celestiaorg/celestia-node/service/header/pb"
	"github.com/celestiaorg/go-libp2p-messenger/serde"
)

var exchangeProtocolID = protocol.ID("/header-exchange/v0.0.1")

// exchange enables sending outbound ExtendedHeaderRequests as well as
// handling inbound ExtendedHeaderRequests.
type exchange struct {
	host host.Host
	// TODO @renaynay: post-Devnet, we need to remove reliance of Exchange on one bootstrap peer
	// Ref https://github.com/celestiaorg/celestia-node/issues/172#issuecomment-964306823.
	peer  peer.ID
	store Store

	ctx    context.Context
	cancel context.CancelFunc
}

func NewExchange(host host.Host, peer peer.ID, store Store) Exchange {
	return &exchange{
		host:  host,
		peer:  peer,
		store: store,
	}
}

func (ex *exchange) Start(context.Context) error {
	ex.ctx, ex.cancel = context.WithCancel(context.Background())
	ex.host.SetStreamHandler(exchangeProtocolID, ex.requestHandler)
	return nil
}

func (ex *exchange) Stop(context.Context) error {
	ex.cancel()
	ex.host.RemoveStreamHandler(exchangeProtocolID)
	return nil
}

// requestHandler handles inbound ExtendedHeaderRequests.
func (ex *exchange) requestHandler(stream network.Stream) {
	// unmarshal request
	pbreq := new(pb.ExtendedHeaderRequest)
	_, err := serde.Read(stream, pbreq)
	if err != nil {
		log.Errorw("reading header request from stream", "err", err)
		stream.Reset() //nolint:errcheck
		return
	}
	// retrieve and write ExtendedHeaders
	if pbreq.Hash != nil {
		ex.handleRequestByHash(pbreq.Hash, stream)
	} else {
		ex.handleRequest(pbreq.Origin, pbreq.Origin+pbreq.Amount, stream)
	}

	err = stream.Close()
	if err != nil {
		log.Errorw("while closing inbound stream", "err", err)
	}
}

func (ex *exchange) handleRequestByHash(hash []byte, stream network.Stream) {
	log.Debugw("handling header request", "hash", tmbytes.HexBytes(hash).String())

	header, err := ex.store.Get(ex.ctx, hash)
	if err != nil {
		log.Errorw("getting header by hash", "hash", tmbytes.HexBytes(hash).String(), "err")
		stream.Reset() //nolint:errcheck
		return
	}
	resp, err := ExtendedHeaderToProto(header)
	if err != nil {
		log.Errorw("marshaling header to proto", "hash", tmbytes.HexBytes(hash).String(), "err")
		stream.Reset() //nolint:errcheck
		return
	}
	_, err = serde.Write(stream, resp)
	if err != nil {
		log.Errorw("writing header to stream", "hash", tmbytes.HexBytes(hash).String(), "err")
		stream.Reset() //nolint:errcheck
		return
	}
}

// handleRequest fetches the ExtendedHeader at the given origin and
// writes it to the stream.
func (ex *exchange) handleRequest(from, to uint64, stream network.Stream) {
	var headers []*ExtendedHeader
	if from == uint64(0) {
		log.Debug("handling head request")

		head, err := ex.store.Head(ex.ctx)
		if err != nil {
			log.Errorw("getting head", "err", err)
			stream.Reset() //nolint:errcheck
			return
		}
		headers = make([]*ExtendedHeader, 1)
		headers[0] = head
	} else {
		log.Debugw("handling headers request", "from", from, "to", to)

		headersByRange, err := ex.store.GetRangeByHeight(ex.ctx, from, to)
		if err != nil {
			log.Errorw("getting headers", "from", from, "to", to, "err", err)
			stream.Reset() //nolint:errcheck
			return
		}
		headers = headersByRange
	}
	// write all headers to stream
	for _, header := range headers {
		resp, err := ExtendedHeaderToProto(header)
		if err != nil {
			log.Errorw("marshaling header to proto", "height", header.Height, "err", err)
			stream.Reset() //nolint:errcheck
			return
		}

		_, err = serde.Write(stream, resp)
		if err != nil {
			log.Errorw("writing header to stream", "height", header.Height, "err", err)
			stream.Reset() //nolint:errcheck
			return
		}
	}
}

func (ex *exchange) RequestHead(ctx context.Context) (*ExtendedHeader, error) {
	log.Debug("requesting head")
	// create request
	req := &pb.ExtendedHeaderRequest{
		Origin: uint64(0),
		Amount: 1,
	}
	headers, err := ex.performRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	return headers[0], nil
}

func (ex *exchange) RequestHeader(ctx context.Context, height uint64) (*ExtendedHeader, error) {
	log.Debugw("requesting header", "height", height)
	// sanity check height
	if height == 0 {
		return nil, fmt.Errorf("specified request height must be greater than 0")
	}
	// create request
	req := &pb.ExtendedHeaderRequest{
		Origin: height,
		Amount: 1,
	}
	headers, err := ex.performRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	return headers[0], nil
}

func (ex *exchange) RequestHeaders(ctx context.Context, from, amount uint64) ([]*ExtendedHeader, error) {
	log.Debugw("requesting headers", "from", from, "to", from+amount)
	// create request
	req := &pb.ExtendedHeaderRequest{
		Origin: from,
		Amount: amount,
	}
	return ex.performRequest(ctx, req)
}

func (ex *exchange) RequestByHash(ctx context.Context, hash tmbytes.HexBytes) (*ExtendedHeader, error) {
	log.Debugw("requesting header", "hash", hash.String())
	// create request
	req := &pb.ExtendedHeaderRequest{
		Hash:   hash.Bytes(),
		Amount: 1,
	}
	headers, err := ex.performRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	return headers[0], nil
}

func (ex *exchange) performRequest(ctx context.Context, req *pb.ExtendedHeaderRequest) ([]*ExtendedHeader, error) {
	stream, err := ex.host.NewStream(ctx, ex.peer, exchangeProtocolID)
	if err != nil {
		return nil, err
	}
	// send request
	_, err = serde.Write(stream, req)
	if err != nil {
		stream.Reset() //nolint:errcheck
		return nil, err
	}
	// read responses
	headers := make([]*ExtendedHeader, req.Amount)
	for i := 0; i < int(req.Amount); i++ {
		resp := new(pb.ExtendedHeader)
		_, err := serde.Read(stream, resp)
		if err != nil {
			stream.Reset() //nolint:errcheck
			return nil, err
		}

		header, err := ProtoToExtendedHeader(resp)
		if err != nil {
			stream.Reset() //nolint:errcheck
			return nil, err
		}

		headers[i] = header
	}
	// ensure at least one header was retrieved
	if len(headers) == 0 {
		return nil, ErrNotFound
	}
	return headers, stream.Close()
}
