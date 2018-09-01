package node

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"sync"

	inet "gx/ipfs/QmQSbtGXCyNrj34LWL8EgXyNNYDZ8r3SwQcpW5pPxVhLnM/go-libp2p-net"
	cbor "gx/ipfs/QmV6BQ6fFCf9eFHDuRxvguvqfKLZtZrxthgZvDfRCs4tMN/go-ipld-cbor"
	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	"gx/ipfs/QmZFbDTY9jfSBms2MchvYM9oYRbAF19K7Pby47yDBfpPrb/go-cid"
	"gx/ipfs/QmZNkThpqfVXs9GNbexPrfBbXSLNYeKrE7jwFM2oqHbyqN/go-libp2p-protocol"

	dag "gx/ipfs/QmeLG6jF1xvEmHca5Vy4q4EdQWp8Xq9S6EPyZrN9wvSRLC/go-merkledag"

	"github.com/filecoin-project/go-filecoin/abi"
	"github.com/filecoin-project/go-filecoin/actor/builtin/storagemarket"
	"github.com/filecoin-project/go-filecoin/address"
	cbu "github.com/filecoin-project/go-filecoin/cborutil"
	"github.com/filecoin-project/go-filecoin/chain"
	"github.com/filecoin-project/go-filecoin/core"
	"github.com/filecoin-project/go-filecoin/crypto"
	"github.com/filecoin-project/go-filecoin/state"
	"github.com/filecoin-project/go-filecoin/vm"
)

// MakeDealProtocolID is the protocol ID for the make deal protocol
const MakeDealProtocolID = protocol.ID("/fil/deal/mk/1.0.0")

// QueryDealProtocolID is the protocol ID for the query deal protocol
const QueryDealProtocolID = protocol.ID("/fil/deal/qry/1.0.0")

func init() {
	cbor.RegisterCborType(DealProposal{})
	cbor.RegisterCborType(DealResponse{})
	cbor.RegisterCborType(DealQuery{})
}

// DealProposal is used for a storage client to propose a deal. It is up to the
// creator of the proposal to select a bid and an ask and turn that into a
// deal, add a reference to the data they want stored to the deal,  then add
// their signature over the deal.
type DealProposal struct {
	Deal      *storagemarket.Deal
	ClientSig crypto.Signature
}

// NewDealProposal will return a DealProposal with a signature derived from Address `addr`
// and Signer `s`. If the address is unknown to the signer an error is returned.
func NewDealProposal(deal *storagemarket.Deal, signer crypto.Signer, addr address.Address) (*DealProposal, error) {
	sig, err := storagemarket.SignDeal(deal, signer, addr)
	if err != nil {
		return nil, err
	}

	return &DealProposal{
		Deal:      deal,
		ClientSig: sig,
	}, nil
}

// LogTag returns the appropriate tag name for a `DealProposal`
func (dp *DealProposal) LogTag() string {
	return "dealProposal"
}

// DealQuery is used to query the state of a deal by its broker generated ID
type DealQuery struct {
	ID [32]byte
}

// DealResponse is returned from the miner after a deal proposal or a deal query
type DealResponse struct {
	// State is the current state of the referenced deal
	State DealState

	// Message is an informational string to aid in interpreting the State. It
	// should not be relied on for any system logic.
	Message string

	// MsgCid is the cid of the 'addDeal' message once the deal is accepted and
	// posted to the blockchain
	MsgCid *cid.Cid

	// ID is an identifying string generated by the miner to track this
	// deal-in-progress
	ID [32]byte
}

// LogTag returns the appropriate tag name for a `DealResponse`
func (dr *DealResponse) LogTag() string {
	return "dealResponse"
}

// StorageBroker manages making storage deals with clients
type StorageBroker struct {
	// TODO: don't depend directly on the node once we find out exactly the set
	// of things we need from it. blah blah passing in function closure nonsense blah blah
	nd *Node

	deals struct {
		set map[[32]byte]*Negotiation
		sync.Mutex
	}

	// smi allows the StorageBroker to fetch data on asks bids and deals from
	// the blockchain (or some mocked source for testing)
	smi storageMarketPeeker
}

// DealState signifies the state of a deal
type DealState int

const (
	// Unknown signifies an unknown negotiation
	Unknown = DealState(iota)

	// Rejected means the deal was rejected for some reason
	Rejected

	// Accepted means the deal was accepted but hasnt yet started
	Accepted

	// Started means the deal has started and the transfer is in progress
	Started

	// Failed means the deal has failed for some reason
	Failed

	// Posted means the deal has been posted to the blockchain
	Posted

	// Complete means the deal is complete
	// TODO: distinguish this from 'Posted'
	Complete

	// Staged means that the data in the deal has been staged into a sector
	Staged
)

func (s DealState) String() string {
	switch s {
	case Unknown:
		return "unknown"
	case Rejected:
		return "rejected"
	case Accepted:
		return "accepted"
	case Started:
		return "started"
	case Failed:
		return "failed"
	case Posted:
		return "posted"
	case Complete:
		return "complete"
	default:
		return fmt.Sprintf("<unrecognized %d>", s)
	}
}

// Negotiation tracks an in-progress deal between a miner and a storage client
type Negotiation struct {
	DealProposal *DealProposal
	MsgCid       *cid.Cid
	State        DealState
	Error        string

	// MinerOwner is the owner of the miner in this deals ask. It is controlled
	// by this nodes operator.
	MinerOwner address.Address
}

// NewStorageBroker sets up a new storage market protocol handler and registers
// it with libp2p
func NewStorageBroker(nd *Node) *StorageBroker {
	sm := &StorageBroker{
		nd:  nd,
		smi: &stateTreeMarketPeeker{nd},
	}
	sm.deals.set = make(map[[32]byte]*Negotiation)

	nd.Host.SetStreamHandler(MakeDealProtocolID, sm.handleNewStreamPropose)
	nd.Host.SetStreamHandler(QueryDealProtocolID, sm.handleNewStreamQuery)

	return sm
}

// ProposeDeal the handler for incoming deal proposals
func (sm *StorageBroker) ProposeDeal(ctx context.Context, propose *DealProposal) (dr *DealResponse, err error) {
	ctx = log.Start(ctx, "StorageMarket.ProposeDeal")
	log.SetTag(ctx, propose.LogTag(), propose)
	defer func() {
		if dr != nil {
			log.SetTag(ctx, dr.LogTag(), dr)
		}
		log.FinishWithErr(ctx, err)
	}()

	ask, err := sm.smi.GetStorageAsk(ctx, propose.Deal.Ask)
	if err != nil {
		return &DealResponse{
			Message: fmt.Sprintf("unknown ask: %s", err),
			State:   Rejected,
		}, nil
	}

	bid, err := sm.smi.GetBid(ctx, propose.Deal.Bid)
	if err != nil {
		return &DealResponse{
			Message: fmt.Sprintf("unknown bid: %s", err),
			State:   Rejected,
		}, nil
	}

	// TODO: also validate that the bids and asks are not expired
	if bid.Used {
		return &DealResponse{
			Message: "bid already used",
			State:   Rejected,
		}, nil
	}

	mowner, err := sm.smi.GetMinerOwner(context.TODO(), ask.Owner)
	if err != nil {
		// TODO: does this get a response? This means that we have an ask whose
		// miner we couldnt look up. Feels like an invariant being invalidated,
		// or a system fault.
		return nil, err
	}

	if !sm.nd.Wallet.HasAddress(mowner) {
		return &DealResponse{
			Message: "ask in deal proposal does not belong to us",
			State:   Rejected,
		}, nil
	}

	if bid.Size.GreaterThan(ask.Size) {
		return &DealResponse{
			Message: "ask does not have enough space for bid",
			State:   Rejected,
		}, nil
	}

	if !storagemarket.VerifyDealSignature(propose.Deal, propose.ClientSig, bid.Owner) {
		return &DealResponse{
			Message: "invalid client signature",
			State:   Rejected,
		}, nil
	}

	// TODO: validate pairing of bid and ask
	// TODO: ensure bid and ask arent already part of a deal we have accepted

	// TODO: don't always auto accept, we should be able to expose this choice to the user
	// TODO: even under 'auto accept', have some restrictions around minimum
	// price and requested collateral.

	id := negotiationID(mowner, propose)
	sm.deals.Lock()
	defer sm.deals.Unlock()

	oneg, ok := sm.deals.set[id]
	if ok {
		return &DealResponse{
			Message: "deal negotiation already in progress",
			State:   oneg.State,
			ID:      id,
		}, nil
	}

	neg := &Negotiation{
		DealProposal: propose,
		State:        Accepted,
		MinerOwner:   mowner,
	}

	sm.deals.set[id] = neg

	// TODO: put this into a scheduler
	go sm.processDeal(id)

	return &DealResponse{
		State: Accepted,
		ID:    id,
	}, nil
}

func (sm *StorageBroker) updateNegotiation(id [32]byte, op func(*Negotiation)) {
	sm.deals.Lock()
	defer sm.deals.Unlock()

	op(sm.deals.set[id])
}

func (sm *StorageBroker) handleNewStreamPropose(s inet.Stream) {
	ctx := context.TODO()
	defer s.Close() // nolint: errcheck
	r := cbu.NewMsgReader(s)
	w := cbu.NewMsgWriter(s)

	var propose DealProposal
	if err := r.ReadMsg(&propose); err != nil {
		s.Reset() // nolint: errcheck
		log.Warningf("failed to read DealProposal: %s", err)
		return
	}

	resp, err := sm.ProposeDeal(ctx, &propose)
	if err != nil {
		s.Reset() // nolint: errcheck
		// TODO: metrics, more structured logging. This is fairly useful information
		log.Infof("incoming deal proposal failed: %s", err)
	}

	if err := w.WriteMsg(resp); err != nil {
		log.Warningf("failed to write back deal propose response: %s", err)
	}
}

func (sm *StorageBroker) handleNewStreamQuery(s inet.Stream) {
	ctx := context.TODO()
	defer s.Close() // nolint: errcheck
	r := cbu.NewMsgReader(s)
	w := cbu.NewMsgWriter(s)

	var q DealQuery
	if err := r.ReadMsg(&q); err != nil {
		s.Reset() // nolint: errcheck
		log.Warningf("failed to read deal query: %s", err)
		return
	}

	resp, err := sm.QueryDeal(ctx, q.ID)
	if err != nil {
		s.Reset() // nolint: errcheck
		// TODO: metrics, more structured logging. This is fairly useful information
		log.Infof("incoming deal query failed: %s", err)
	}

	if err := w.WriteMsg(resp); err != nil {
		log.Warningf("failed to write back deal query response: %s", err)
	}
}

// QueryDeal is the handler for incoming deal queries
func (sm *StorageBroker) QueryDeal(ctx context.Context, id [32]byte) (dr *DealResponse, err error) {
	ctx = log.Start(ctx, "StorageMarket.QueryDeal")
	log.SetTag(ctx, "id", id)
	defer func() {
		if dr != nil {
			log.SetTag(ctx, dr.LogTag(), dr)
		}
		log.FinishWithErr(ctx, err)
	}()
	sm.deals.Lock()
	defer sm.deals.Unlock()

	neg, ok := sm.deals.set[id]
	if !ok {
		return &DealResponse{State: Unknown}, nil
	}

	return &DealResponse{
		State:   neg.State,
		Message: neg.Error,
		MsgCid:  neg.MsgCid,
	}, nil
}

func negotiationID(minerID address.Address, propose *DealProposal) [32]byte {
	data, err := cbor.DumpObject(propose)
	if err != nil {
		panic(err)
	}

	data = append(data, minerID[:]...)

	return sha256.Sum256(data)
}

func (sm *StorageBroker) processDeal(id [32]byte) {
	var propose *DealProposal
	var minerOwner address.Address
	sm.updateNegotiation(id, func(n *Negotiation) {
		propose = n.DealProposal
		minerOwner = n.MinerOwner
		n.State = Started
	})

	msgcid, err := sm.finishDeal(context.TODO(), minerOwner, propose)
	if err != nil {
		log.Warning(err)
		sm.updateNegotiation(id, func(n *Negotiation) {
			n.State = Failed
			n.Error = err.Error()
		})
		return
	}

	sm.updateNegotiation(id, func(n *Negotiation) {
		n.MsgCid = msgcid
		n.State = Posted
	})
}

func (sm *StorageBroker) finishDeal(ctx context.Context, minerOwner address.Address, propose *DealProposal) (*cid.Cid, error) {
	// TODO: better file fetching
	dataRef, err := cid.Decode(propose.Deal.DataRef)
	if err != nil {
		return nil, errors.Wrap(err, "corrupt cid in deal")
	}

	if err := sm.fetchData(context.TODO(), dataRef); err != nil {
		return nil, errors.Wrap(err, "fetching data failed")
	}

	msgcid, err := sm.smi.AddDeal(ctx, minerOwner, propose.Deal.Ask, propose.Deal.Bid, propose.ClientSig, dataRef)
	if err != nil {
		return nil, err
	}

	return msgcid, nil
}

func (sm *StorageBroker) fetchData(ctx context.Context, ref *cid.Cid) error {
	return dag.FetchGraph(ctx, ref, dag.NewDAGService(sm.nd.Blockservice))
}

// GetMarketPeeker returns the storageMarketPeeker for this storage market
// TODO: something something returning unexported interfaces?
func (sm *StorageBroker) GetMarketPeeker() storageMarketPeeker { // nolint: golint
	return sm.smi
}

type storageMarketPeeker interface {
	GetStorageAsk(ctx context.Context, id uint64) (*storagemarket.Ask, error)
	GetBid(ctx context.Context, id uint64) (*storagemarket.Bid, error)
	AddDeal(ctx context.Context, from address.Address, bid, ask uint64, sig crypto.Signature, data *cid.Cid) (*cid.Cid, error)

	// more of a gape than a peek..
	GetStorageAskSet(ctx context.Context) (storagemarket.AskSet, error)
	GetBidSet(ctx context.Context) (storagemarket.BidSet, error)
	GetMinerOwner(context.Context, address.Address) (address.Address, error)
}

type stateTreeMarketPeeker struct {
	nd *Node
}

func (stsa *stateTreeMarketPeeker) loadStateTree(ctx context.Context) (state.Tree, error) {
	ts := stsa.nd.ChainMgr.GetHeaviestTipSet()
	return stsa.nd.ChainMgr.State(ctx, ts.ToSlice())
}

func (stsa *stateTreeMarketPeeker) queryMessage(ctx context.Context, addr address.Address, method string, params ...interface{}) ([][]byte, error) {
	vals, err := abi.ToValues(params)
	if err != nil {
		return nil, err
	}

	args, err := abi.EncodeValues(vals)
	if err != nil {
		return nil, err
	}

	st, err := stsa.loadStateTree(ctx)
	if err != nil {
		return nil, err
	}

	vms := vm.NewStorageMap(stsa.nd.Blockstore)
	rets, ec, err := core.CallQueryMethod(ctx, st, vms, addr, method, args, address.Address{}, nil)
	if err != nil {
		return nil, err
	}

	if ec != 0 {
		return nil, errors.Errorf("Non-zero return code from query message: %d", ec)
	}

	return rets, nil
}

// GetAsk returns the given ask from the current state of the storage market actor
func (stsa *stateTreeMarketPeeker) GetStorageAsk(ctx context.Context, id uint64) (a *storagemarket.Ask, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.GetStorageAsk")
	log.SetTag(ctx, "id", id)
	defer func() {
		if a != nil {
			log.SetTag(ctx, "ask", a)
		}
		log.FinishWithErr(ctx, err)
	}()

	var ask storagemarket.Ask

	rets, err := stsa.queryMessage(ctx, address.StorageMarketAddress, "getAsk", big.NewInt(int64(id)))
	if err != nil {
		return nil, err
	}

	if err := cbor.DecodeInto(rets[0], &ask); err != nil {
		return nil, err
	}

	return &ask, nil
}

// GetBid returns the given bid from the current state of the storage market actor
func (stsa *stateTreeMarketPeeker) GetBid(ctx context.Context, id uint64) (b *storagemarket.Bid, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.GetBid")
	log.SetTag(ctx, "id", id)
	defer func() {
		if b != nil {
			log.SetTag(ctx, "bid", b)
		}
		log.FinishWithErr(ctx, err)
	}()

	var bid storagemarket.Bid

	rets, err := stsa.queryMessage(context.TODO(), address.StorageMarketAddress, "getBid", big.NewInt(int64(id)))
	if err != nil {
		return nil, err
	}

	err = cbor.DecodeInto(rets[0], &bid)
	if err != nil {
		return nil, err
	}

	return &bid, nil
}

// GetAskSet returns the given the entire ask set from the storage market
// TODO limit number of results
func (stsa *stateTreeMarketPeeker) GetStorageAskSet(ctx context.Context) (as storagemarket.AskSet, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.GetStorageAskSet")
	defer func() {
		if as != nil {
			log.SetTag(ctx, "askSet", as)
		}
		log.FinishWithErr(ctx, err)
	}()

	askSet := storagemarket.AskSet{}

	rets, err := stsa.queryMessage(context.TODO(), address.StorageMarketAddress, "getAllAsks")
	if err != nil {
		return nil, err
	}

	if err = cbor.DecodeInto(rets[0], &askSet); err != nil {
		return nil, err
	}

	return askSet, nil
}

// GetBidSet returns the given the entire bid set from the storage market
// TODO limit number of results
func (stsa *stateTreeMarketPeeker) GetBidSet(ctx context.Context) (bs storagemarket.BidSet, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.GetBidSet")
	defer func() {
		if bs != nil {
			log.SetTag(ctx, "bidSet", bs)
		}
		log.FinishWithErr(ctx, err)
	}()

	bidSet := storagemarket.BidSet{}

	rets, err := stsa.queryMessage(context.TODO(), address.StorageMarketAddress, "getAllBids")
	if err != nil {
		return nil, err
	}

	err = cbor.DecodeInto(rets[0], &bidSet)
	if err != nil {
		return nil, err
	}

	return bidSet, nil
}

func (stsa *stateTreeMarketPeeker) GetMinerOwner(ctx context.Context, minerAddress address.Address) (a address.Address, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.GetMinerOwner")
	log.SetTag(ctx, "minerAddress", minerAddress.String())
	defer func() {
		log.SetTag(ctx, "ownerAddress", a)
		log.FinishWithErr(ctx, err)
	}()

	rets, err := stsa.queryMessage(ctx, minerAddress, "getOwner")
	if err != nil {
		return address.Address{}, err
	}

	addr, err := address.NewFromBytes(rets[0])
	if err != nil {
		return address.Address{}, err
	}

	return addr, nil
}

// AddDeal adds a deal by sending a message to the storage market actor on chain
func (stsa *stateTreeMarketPeeker) AddDeal(ctx context.Context, from address.Address, ask, bid uint64, sig crypto.Signature, data *cid.Cid) (c *cid.Cid, err error) {
	ctx = log.Start(ctx, "StorageMarketPeerker.AddDeal")
	log.SetTags(ctx, map[string]interface{}{
		"from": from.String(),
		"ask":  ask,
		"bid":  bid,
		"sig":  sig,
		"data": data.String(),
	})
	defer func() {
		if c != nil {
			log.SetTag(ctx, "dealCid", c.String())
		}
		log.FinishWithErr(ctx, err)
	}()

	pdata, err := abi.ToEncodedValues(big.NewInt(0).SetUint64(ask), big.NewInt(0).SetUint64(bid), []byte(sig), data.Bytes())
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode abi values")
	}

	msg, err := NewMessageWithNextNonce(ctx, stsa.nd, from, address.StorageMarketAddress, nil, "addDeal", pdata)
	if err != nil {
		return nil, err
	}

	smsg, err := chain.NewSignedMessage(*msg, stsa.nd.Wallet)
	if err != nil {
		return nil, err
	}

	err = stsa.nd.AddNewMessage(ctx, smsg)
	if err != nil {
		return nil, errors.Wrap(err, "sending 'addDeal' message failed")
	}

	return smsg.Cid()
}
