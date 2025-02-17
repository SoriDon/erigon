package p2p

import (
	"context"
	"fmt"
	"sync"

	"github.com/ledgerwatch/log/v3"
	"google.golang.org/grpc"

	"github.com/ledgerwatch/erigon-lib/direct"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/sentry"
	"github.com/ledgerwatch/erigon/eth/protocols/eth"
	sentrymulticlient "github.com/ledgerwatch/erigon/p2p/sentry/sentry_multi_client"
	"github.com/ledgerwatch/erigon/rlp"
)

type DecodedInboundMessage[TPacket any] struct {
	*sentry.InboundMessage
	Decoded TPacket
	PeerId  *PeerId
}

type MessageObserver[TMessage any] func(message TMessage)

type UnregisterFunc func()

type MessageListener interface {
	Run(ctx context.Context)
	RegisterNewBlockObserver(observer MessageObserver[*DecodedInboundMessage[*eth.NewBlockPacket]]) UnregisterFunc
	RegisterNewBlockHashesObserver(observer MessageObserver[*DecodedInboundMessage[*eth.NewBlockHashesPacket]]) UnregisterFunc
	RegisterBlockHeadersObserver(observer MessageObserver[*DecodedInboundMessage[*eth.BlockHeadersPacket66]]) UnregisterFunc
	RegisterPeerEventObserver(observer MessageObserver[*sentry.PeerEvent]) UnregisterFunc
}

func NewMessageListener(logger log.Logger, sentryClient direct.SentryClient, peerPenalizer PeerPenalizer) MessageListener {
	return newMessageListener(logger, sentryClient, peerPenalizer)
}

func newMessageListener(logger log.Logger, sentryClient direct.SentryClient, peerPenalizer PeerPenalizer) *messageListener {
	return &messageListener{
		logger:                  logger,
		sentryClient:            sentryClient,
		peerPenalizer:           peerPenalizer,
		newBlockObservers:       map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockPacket]]{},
		newBlockHashesObservers: map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockHashesPacket]]{},
		blockHeadersObservers:   map[uint64]MessageObserver[*DecodedInboundMessage[*eth.BlockHeadersPacket66]]{},
		peerEventObservers:      map[uint64]MessageObserver[*sentry.PeerEvent]{},
	}
}

type messageListener struct {
	once                    sync.Once
	observerIdSequence      uint64
	logger                  log.Logger
	sentryClient            direct.SentryClient
	peerPenalizer           PeerPenalizer
	observersMu             sync.Mutex
	newBlockObservers       map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockPacket]]
	newBlockHashesObservers map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockHashesPacket]]
	blockHeadersObservers   map[uint64]MessageObserver[*DecodedInboundMessage[*eth.BlockHeadersPacket66]]
	peerEventObservers      map[uint64]MessageObserver[*sentry.PeerEvent]
	stopWg                  sync.WaitGroup
}

func (ml *messageListener) Run(ctx context.Context) {
	backgroundLoops := []func(ctx context.Context){
		ml.listenInboundMessages,
		ml.listenPeerEvents,
	}

	ml.stopWg.Add(len(backgroundLoops))
	for _, loop := range backgroundLoops {
		go loop(ctx)
	}

	<-ctx.Done()
	// once context has been cancelled wait for the background loops to stop
	ml.stopWg.Wait()

	// unregister all observers
	ml.observersMu.Lock()
	defer ml.observersMu.Unlock()
	ml.newBlockObservers = map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockPacket]]{}
	ml.newBlockHashesObservers = map[uint64]MessageObserver[*DecodedInboundMessage[*eth.NewBlockHashesPacket]]{}
	ml.blockHeadersObservers = map[uint64]MessageObserver[*DecodedInboundMessage[*eth.BlockHeadersPacket66]]{}
	ml.peerEventObservers = map[uint64]MessageObserver[*sentry.PeerEvent]{}
}

func (ml *messageListener) RegisterNewBlockObserver(observer MessageObserver[*DecodedInboundMessage[*eth.NewBlockPacket]]) UnregisterFunc {
	return registerObserver(ml, ml.newBlockObservers, observer)
}

func (ml *messageListener) RegisterNewBlockHashesObserver(observer MessageObserver[*DecodedInboundMessage[*eth.NewBlockHashesPacket]]) UnregisterFunc {
	return registerObserver(ml, ml.newBlockHashesObservers, observer)
}

func (ml *messageListener) RegisterBlockHeadersObserver(observer MessageObserver[*DecodedInboundMessage[*eth.BlockHeadersPacket66]]) UnregisterFunc {
	return registerObserver(ml, ml.blockHeadersObservers, observer)
}

func (ml *messageListener) RegisterPeerEventObserver(observer MessageObserver[*sentry.PeerEvent]) UnregisterFunc {
	return registerObserver(ml, ml.peerEventObservers, observer)
}

func (ml *messageListener) listenInboundMessages(ctx context.Context) {
	streamFactory := func(ctx context.Context, sentryClient direct.SentryClient) (sentrymulticlient.SentryMessageStream, error) {
		messagesRequest := sentry.MessagesRequest{
			Ids: []sentry.MessageId{
				sentry.MessageId_NEW_BLOCK_66,
				sentry.MessageId_NEW_BLOCK_HASHES_66,
				sentry.MessageId_BLOCK_HEADERS_66,
			},
		}

		return sentryClient.Messages(ctx, &messagesRequest, grpc.WaitForReady(true))
	}

	streamMessages(ctx, ml, "InboundMessages", streamFactory, func(message *sentry.InboundMessage) error {
		switch message.Id {
		case sentry.MessageId_NEW_BLOCK_66:
			return notifyInboundMessageObservers(ctx, ml, ml.newBlockObservers, message)
		case sentry.MessageId_NEW_BLOCK_HASHES_66:
			return notifyInboundMessageObservers(ctx, ml, ml.newBlockHashesObservers, message)
		case sentry.MessageId_BLOCK_HEADERS_66:
			return notifyInboundMessageObservers(ctx, ml, ml.blockHeadersObservers, message)
		default:
			return nil
		}
	})
}

func (ml *messageListener) listenPeerEvents(ctx context.Context) {
	streamFactory := func(ctx context.Context, sentryClient direct.SentryClient) (sentrymulticlient.SentryMessageStream, error) {
		return sentryClient.PeerEvents(ctx, &sentry.PeerEventsRequest{}, grpc.WaitForReady(true))
	}

	streamMessages(ctx, ml, "PeerEvents", streamFactory, ml.notifyPeerEventObservers)
}

func (ml *messageListener) notifyPeerEventObservers(peerEvent *sentry.PeerEvent) error {
	notifyObservers(&ml.observersMu, ml.peerEventObservers, peerEvent)
	return nil
}

func (ml *messageListener) statusDataFactory() sentrymulticlient.StatusDataFactory {
	return func() *sentry.StatusData {
		// TODO add a "status data component" that message listener will use as a dependency to fetch status data
		//      "status data component" will be responsible for providing a mechanism to provide up-to-date status data
		return &sentry.StatusData{}
	}
}

func (ml *messageListener) nextObserverId() uint64 {
	id := ml.observerIdSequence
	ml.observerIdSequence++
	return id
}

func registerObserver[TMessage any](
	ml *messageListener,
	observers map[uint64]MessageObserver[*TMessage],
	observer MessageObserver[*TMessage],
) UnregisterFunc {
	ml.observersMu.Lock()
	defer ml.observersMu.Unlock()

	observerId := ml.nextObserverId()
	observers[observerId] = observer
	return unregisterFunc(&ml.observersMu, observers, observerId)
}

func unregisterFunc[TMessage any](mu *sync.Mutex, observers map[uint64]MessageObserver[TMessage], observerId uint64) UnregisterFunc {
	return func() {
		mu.Lock()
		defer mu.Unlock()

		delete(observers, observerId)
	}
}

func streamMessages[TMessage any](
	ctx context.Context,
	ml *messageListener,
	name string,
	streamFactory sentrymulticlient.SentryMessageStreamFactory,
	handler func(event *TMessage) error,
) {
	defer ml.stopWg.Done()

	messageHandler := func(_ context.Context, event *TMessage, _ direct.SentryClient) error {
		return handler(event)
	}

	sentrymulticlient.SentryReconnectAndPumpStreamLoop(
		ctx,
		ml.sentryClient,
		ml.statusDataFactory(),
		name,
		streamFactory,
		func() *TMessage { return new(TMessage) },
		messageHandler,
		nil,
		ml.logger,
	)
}

func notifyInboundMessageObservers[TPacket any](
	ctx context.Context,
	ml *messageListener,
	observers map[uint64]MessageObserver[*DecodedInboundMessage[TPacket]],
	message *sentry.InboundMessage,
) error {
	peerId := PeerIdFromH512(message.PeerId)

	var decodedData TPacket
	if err := rlp.DecodeBytes(message.Data, &decodedData); err != nil {
		if rlp.IsInvalidRLPError(err) {
			ml.logger.Debug("penalizing peer", "peerId", peerId, "err", err)

			penalizeErr := ml.peerPenalizer.Penalize(ctx, peerId)
			if penalizeErr != nil {
				err = fmt.Errorf("%w: %w", penalizeErr, err)
			}
		}

		return err
	}

	notifyObservers(&ml.observersMu, observers, &DecodedInboundMessage[TPacket]{
		InboundMessage: message,
		Decoded:        decodedData,
		PeerId:         peerId,
	})

	return nil
}

func notifyObservers[TMessage any](mu *sync.Mutex, observers map[uint64]MessageObserver[TMessage], message TMessage) {
	mu.Lock()
	defer mu.Unlock()

	for _, observer := range observers {
		go observer(message)
	}
}
