package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/breez/lspd/cln_plugin/proto"
	"github.com/breez/lspd/config"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

type ClnHtlcInterceptor struct {
	config        *config.NodeConfig
	pluginAddress string
	client        *ClnClient
	pluginClient  proto.ClnPluginClient
	initWg        sync.WaitGroup
	doneWg        sync.WaitGroup
	stopRequested bool
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewClnHtlcInterceptor(conf *config.NodeConfig) (*ClnHtlcInterceptor, error) {
	if conf.Cln == nil {
		return nil, fmt.Errorf("missing cln config")
	}

	client, err := NewClnClient(conf.Cln.SocketPath)
	if err != nil {
		return nil, err
	}
	i := &ClnHtlcInterceptor{
		config:        conf,
		pluginAddress: conf.Cln.PluginAddress,
		client:        client,
	}

	i.initWg.Add(1)
	return i, nil
}

func (i *ClnHtlcInterceptor) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	log.Printf("Dialing cln plugin on '%s'", i.pluginAddress)
	conn, err := grpc.DialContext(
		ctx,
		i.pluginAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    time.Duration(10) * time.Second,
			Timeout: time.Duration(10) * time.Second,
		}),
	)
	if err != nil {
		log.Printf("grpc.Dial error: %v", err)
		cancel()
		return err
	}

	i.pluginClient = proto.NewClnPluginClient(conn)
	i.ctx = ctx
	i.cancel = cancel
	i.stopRequested = false
	return i.intercept()
}

func (i *ClnHtlcInterceptor) intercept() error {
	inited := false

	defer func() {
		if !inited {
			i.initWg.Done()
		}
		log.Printf("CLN intercept(): stopping. Waiting for in-progress interceptions to complete.")
		i.doneWg.Wait()
	}()

	for {
		if i.ctx.Err() != nil {
			return i.ctx.Err()
		}

		log.Printf("Connecting CLN HTLC interceptor.")
		interceptorClient, err := i.pluginClient.HtlcStream(i.ctx)
		if err != nil {
			log.Printf("pluginClient.HtlcStream(): %v", err)
			<-time.After(time.Second)
			continue
		}

		for {
			if i.ctx.Err() != nil {
				return i.ctx.Err()
			}

			if !inited {
				inited = true
				i.initWg.Done()
			}

			// Stop receiving if stop if requested. The defer func on top of this
			// function will assure all htlcs that are currently being processed
			// will complete.
			if i.stopRequested {
				return nil
			}

			request, err := interceptorClient.Recv()
			if err != nil {
				// If it is  just the error result of the context cancellation
				// the we exit silently.
				status, ok := status.FromError(err)
				if ok && status.Code() == codes.Canceled {
					log.Printf("Got code canceled. Break.")
					break
				}

				// Otherwise it an unexpected error, we fail the test.
				log.Printf("unexpected error in interceptor.Recv() %v", err)
				break
			}
			nextHop := "<unknown>"
			channels, err := i.client.client.GetChannel(request.Onion.ShortChannelId)
			if err != nil {
				for _, c := range channels {
					if c.Source == i.config.NodePubkey {
						nextHop = c.Destination
						break
					}
					if c.Destination == i.config.NodePubkey {
						nextHop = c.Source
						break
					}
				}
			}

			log.Printf("correlationid: %v\nhtlc: %v\nchanID: %v\nnextHop: %v\nincoming amount: %v\noutgoing amount: %v\nincoming expiry: %v\noutgoing expiry: %v\npaymentHash: %v\nonionBlob: %v\n\n",
				request.Correlationid,
				request.Htlc,
				request.Onion.ShortChannelId,
				nextHop,
				request.Htlc.AmountMsat, //with fees
				request.Onion.ForwardMsat,
				request.Htlc.CltvExpiryRelative,
				request.Htlc.CltvExpiry,
				request.Htlc.PaymentHash,
				request,
			)

			i.doneWg.Add(1)
			go func() {
				paymentHash, err := hex.DecodeString(request.Htlc.PaymentHash)
				if err != nil {
					interceptorClient.Send(i.defaultResolution(request))
					i.doneWg.Done()
				}
				interceptResult := intercept(i.client, i.config, nextHop, paymentHash, request.Onion.ForwardMsat, request.Onion.OutgoingCltvValue, request.Htlc.CltvExpiry)
				switch interceptResult.action {
				case INTERCEPT_RESUME_WITH_ONION:
					interceptorClient.Send(i.resumeWithOnion(request, interceptResult))
				case INTERCEPT_FAIL_HTLC_WITH_CODE:
					interceptorClient.Send(
						i.failWithCode(request, interceptResult.failureCode),
					)
				case INTERCEPT_RESUME:
					fallthrough
				default:
					interceptorClient.Send(
						i.defaultResolution(request),
					)
				}

				i.doneWg.Done()
			}()
		}

		<-time.After(time.Second)
	}
}

func (i *ClnHtlcInterceptor) Stop() error {
	// Setting stopRequested to true will make the interceptor stop receiving.
	i.stopRequested = true

	// Wait until all already received htlcs are handled, responses sent back.
	i.doneWg.Wait()

	// Close the grpc connection.
	i.cancel()
	return nil
}

func (i *ClnHtlcInterceptor) WaitStarted() {
	i.initWg.Wait()
}

func (i *ClnHtlcInterceptor) resumeWithOnion(request *proto.HtlcAccepted, interceptResult interceptResult) *proto.HtlcResolution {
	//decoding and encoding onion with alias in type 6 record.
	payload, err := hex.DecodeString(request.Onion.Payload)
	if err != nil {
		log.Printf("resumeWithOnion: hex.DecodeString(%v) error: %v", request.Onion.Payload, err)
		return i.failWithCode(request, FAILURE_TEMPORARY_CHANNEL_FAILURE)
	}
	newPayload, err := encodePayloadWithNextHop(payload, interceptResult.channelId)
	if err != nil {
		log.Printf("encodePayloadWithNextHop error: %v", err)
		return i.failWithCode(request, FAILURE_TEMPORARY_CHANNEL_FAILURE)
	}

	newPayloadStr := hex.EncodeToString(newPayload)

	chanId := lnwire.NewChanIDFromOutPoint(interceptResult.channelPoint).String()
	log.Printf("forwarding htlc to the destination node and a new private channel was opened")
	return &proto.HtlcResolution{
		Correlationid: request.Correlationid,
		Outcome: &proto.HtlcResolution_Continue{
			Continue: &proto.HtlcContinue{
				ForwardTo: &chanId,
				Payload:   &newPayloadStr,
			},
		},
	}
}

func (i *ClnHtlcInterceptor) defaultResolution(request *proto.HtlcAccepted) *proto.HtlcResolution {
	return &proto.HtlcResolution{
		Correlationid: request.Correlationid,
		Outcome: &proto.HtlcResolution_Continue{
			Continue: &proto.HtlcContinue{},
		},
	}
}

func (i *ClnHtlcInterceptor) failWithCode(request *proto.HtlcAccepted, code interceptFailureCode) *proto.HtlcResolution {
	return &proto.HtlcResolution{
		Correlationid: request.Correlationid,
		Outcome: &proto.HtlcResolution_Fail{
			Fail: &proto.HtlcFail{
				Failure: &proto.HtlcFail_FailureMessage{
					FailureMessage: i.mapFailureCode(code),
				},
			},
		},
	}
}

func encodePayloadWithNextHop(payload []byte, channelId uint64) ([]byte, error) {
	bufReader := bytes.NewBuffer(payload)
	var b [8]byte
	varInt, err := sphinx.ReadVarInt(bufReader, &b)
	if err != nil {
		return nil, fmt.Errorf("failed to read payload length %x: %v", payload, err)
	}

	innerPayload := make([]byte, varInt)
	if _, err := io.ReadFull(bufReader, innerPayload[:]); err != nil {
		return nil, fmt.Errorf("failed to decode payload %x: %v", innerPayload[:], err)
	}

	s, _ := tlv.NewStream()
	tlvMap, err := s.DecodeWithParsedTypes(bytes.NewReader(innerPayload))
	if err != nil {
		return nil, fmt.Errorf("DecodeWithParsedTypes failed for %x: %v", innerPayload[:], err)
	}

	tt := record.NewNextHopIDRecord(&channelId)
	buf := bytes.NewBuffer([]byte{})
	if err := tt.Encode(buf); err != nil {
		return nil, fmt.Errorf("failed to encode nexthop %x: %v", innerPayload[:], err)
	}

	uTlvMap := make(map[uint64][]byte)
	for t, b := range tlvMap {
		if t == record.NextHopOnionType {
			uTlvMap[uint64(t)] = buf.Bytes()
			continue
		}
		uTlvMap[uint64(t)] = b
	}
	tlvRecords := tlv.MapToRecords(uTlvMap)
	s, err = tlv.NewStream(tlvRecords...)
	if err != nil {
		return nil, fmt.Errorf("tlv.NewStream(%x) error: %v", tlvRecords, err)
	}
	var newPayloadBuf bytes.Buffer
	err = s.Encode(&newPayloadBuf)
	if err != nil {
		return nil, fmt.Errorf("encode error: %v", err)
	}
	return newPayloadBuf.Bytes(), nil
}

func (i *ClnHtlcInterceptor) mapFailureCode(original interceptFailureCode) string {
	switch original {
	case FAILURE_TEMPORARY_CHANNEL_FAILURE:
		return "1007"
	case FAILURE_TEMPORARY_NODE_FAILURE:
		return "2002"
	case FAILURE_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS:
		return "400F"
	default:
		log.Printf("Unknown failure code %v, default to temporary channel failure.", original)
		return "1007" // temporary channel failure
	}
}
