package p2p

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ledgerwatch/erigon-lib/gointerfaces/sentry"
	"github.com/ledgerwatch/erigon/core/types"
)

func TestPenalizingFetcherFetchHeadersShouldPenalizePeerWhenErrTooManyHeaders(t *testing.T) {
	t.Parallel()

	peerId := PeerIdFromUint64(1)
	requestId := uint64(1234)
	mockInboundMessages := []*sentry.InboundMessage{
		{
			Id:     sentry.MessageId_BLOCK_HEADERS_66,
			PeerId: peerId.H512(),
			// response should contain 2 headers instead we return 5
			Data: newMockBlockHeadersPacket66Bytes(t, requestId, 5),
		},
	}
	mockRequestResponse := requestResponseMock{
		requestId:                   requestId,
		mockResponseInboundMessages: mockInboundMessages,
		wantRequestPeerId:           peerId,
		wantRequestOriginNumber:     1,
		wantRequestAmount:           2,
	}

	test := newPenalizingFetcherTest(t, newMockRequestGenerator(requestId))
	test.mockSentryStreams(mockRequestResponse)
	// setup expectation that peer should be penalized
	mockExpectPenalizePeer(t, test.sentryClient, peerId)
	test.run(func(ctx context.Context, t *testing.T) {
		var errTooManyHeaders *ErrTooManyHeaders
		headers, err := test.penalizingFetcher.FetchHeaders(ctx, 1, 3, peerId)
		require.ErrorAs(t, err, &errTooManyHeaders)
		require.Equal(t, 2, errTooManyHeaders.requested)
		require.Equal(t, 5, errTooManyHeaders.received)
		require.Nil(t, headers)
	})
}

func TestPenalizingFetcherFetchHeadersShouldPenalizePeerWhenErrNonSequentialHeaderNumbers(t *testing.T) {
	t.Parallel()

	peerId := PeerIdFromUint64(1)
	requestId := uint64(1234)
	mockBlockHeaders := newMockBlockHeaders(5)
	disconnectedHeaders := make([]*types.Header, 3)
	disconnectedHeaders[0] = mockBlockHeaders[0]
	disconnectedHeaders[1] = mockBlockHeaders[2]
	disconnectedHeaders[2] = mockBlockHeaders[4]
	mockInboundMessages := []*sentry.InboundMessage{
		{
			Id:     sentry.MessageId_BLOCK_HEADERS_66,
			PeerId: peerId.H512(),
			Data:   blockHeadersPacket66Bytes(t, requestId, disconnectedHeaders),
		},
	}
	mockRequestResponse := requestResponseMock{
		requestId:                   requestId,
		mockResponseInboundMessages: mockInboundMessages,
		wantRequestPeerId:           peerId,
		wantRequestOriginNumber:     1,
		wantRequestAmount:           3,
	}

	test := newPenalizingFetcherTest(t, newMockRequestGenerator(requestId))
	test.mockSentryStreams(mockRequestResponse)
	// setup expectation that peer should be penalized
	mockExpectPenalizePeer(t, test.sentryClient, peerId)
	test.run(func(ctx context.Context, t *testing.T) {
		var errNonSequentialHeaderNumbers *ErrNonSequentialHeaderNumbers
		headers, err := test.penalizingFetcher.FetchHeaders(ctx, 1, 4, peerId)
		require.ErrorAs(t, err, &errNonSequentialHeaderNumbers)
		require.Equal(t, uint64(3), errNonSequentialHeaderNumbers.current)
		require.Equal(t, uint64(2), errNonSequentialHeaderNumbers.expected)
		require.Nil(t, headers)
	})
}

func TestPenalizingFetcherFetchHeadersShouldPenalizePeerWhenIncorrectOrigin(t *testing.T) {
	t.Parallel()

	peerId := PeerIdFromUint64(1)
	requestId := uint64(1234)
	mockBlockHeaders := newMockBlockHeaders(3)
	incorrectOriginHeaders := mockBlockHeaders[1:]
	mockInboundMessages := []*sentry.InboundMessage{
		{
			Id:     sentry.MessageId_BLOCK_HEADERS_66,
			PeerId: peerId.H512(),
			// response headers should be 2 and start at 1 - instead we start at 2
			Data: blockHeadersPacket66Bytes(t, requestId, incorrectOriginHeaders),
		},
	}
	mockRequestResponse := requestResponseMock{
		requestId:                   requestId,
		mockResponseInboundMessages: mockInboundMessages,
		wantRequestPeerId:           peerId,
		wantRequestOriginNumber:     1,
		wantRequestAmount:           2,
	}

	test := newPenalizingFetcherTest(t, newMockRequestGenerator(requestId))
	test.mockSentryStreams(mockRequestResponse)
	// setup expectation that peer should be penalized
	mockExpectPenalizePeer(t, test.sentryClient, peerId)
	test.run(func(ctx context.Context, t *testing.T) {
		var errNonSequentialHeaderNumbers *ErrNonSequentialHeaderNumbers
		headers, err := test.penalizingFetcher.FetchHeaders(ctx, 1, 3, peerId)
		require.ErrorAs(t, err, &errNonSequentialHeaderNumbers)
		require.Equal(t, uint64(2), errNonSequentialHeaderNumbers.current)
		require.Equal(t, uint64(1), errNonSequentialHeaderNumbers.expected)
		require.Nil(t, headers)
	})
}

func newPenalizingFetcherTest(t *testing.T, requestIdGenerator RequestIdGenerator) *penalizingFetcherTest {
	fetcherTest := newFetcherTest(t, requestIdGenerator)
	penalizingFetcher := newPenalizingFetcher(fetcherTest.logger, fetcherTest.fetcher, NewPeerPenalizer(fetcherTest.sentryClient))
	return &penalizingFetcherTest{
		fetcherTest:       fetcherTest,
		penalizingFetcher: penalizingFetcher,
	}
}

type penalizingFetcherTest struct {
	*fetcherTest
	penalizingFetcher *penalizingFetcher
}
