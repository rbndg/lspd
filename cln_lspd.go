package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	lspdrpc "github.com/breez/lspd/rpc"
	"github.com/btcsuite/btcd/btcec"
	"github.com/golang/protobuf/proto"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/niftynei/glightning/glightning"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type server_c struct{}

var (
	cln_privateKey *btcec.PrivateKey
	cln_publicKey  *btcec.PublicKey
)

//grpc clightning server

func (s *server_c) ChannelInformation(ctx context.Context, in *lspdrpc.ChannelInformationRequest) (*lspdrpc.ChannelInformationReply, error) {
	return &lspdrpc.ChannelInformationReply{
		Name:                  os.Getenv("CLN_NODE_NAME"),
		Pubkey:                os.Getenv("CLN_NODE_PUBKEY"),
		Host:                  os.Getenv("CLN_NODE_HOST"),
		ChannelCapacity:       publicChannelAmount,
		TargetConf:            targetConf,
		MinHtlcMsat:           minHtlcMsat,
		BaseFeeMsat:           baseFeeMsat,
		FeeRate:               feeRate,
		TimeLockDelta:         timeLockDelta,
		ChannelFeePermyriad:   channelFeePermyriad,
		ChannelMinimumFeeMsat: channelMinimumFeeMsat,
		LspPubkey:             cln_publicKey.SerializeCompressed(),
		MaxInactiveDuration:   maxInactiveDuration,
	}, nil
}

func (s *server_c) RegisterPayment(ctx context.Context, in *lspdrpc.RegisterPaymentRequest) (*lspdrpc.RegisterPaymentReply, error) {
	data, err := btcec.Decrypt(cln_privateKey, in.Blob)
	if err != nil {
		log.Printf("btcec.Decrypt(%x) error: %v", in.Blob, err)
		return nil, fmt.Errorf("btcec.Decrypt(%x) error: %w", in.Blob, err)
	}
	var pi lspdrpc.PaymentInformation
	err = proto.Unmarshal(data, &pi)
	if err != nil {
		log.Printf("proto.Unmarshal(%x) error: %v", data, err)
		return nil, fmt.Errorf("proto.Unmarshal(%x) error: %w", data, err)
	}
	log.Printf("RegisterPayment - Destination: %x, pi.PaymentHash: %x, pi.PaymentSecret: %x, pi.IncomingAmountMsat: %v, pi.OutgoingAmountMsat: %v",
		pi.Destination, pi.PaymentHash, pi.PaymentSecret, pi.IncomingAmountMsat, pi.OutgoingAmountMsat)
	err = checkPayment(pi.IncomingAmountMsat, pi.OutgoingAmountMsat)
	if err != nil {
		log.Printf("checkPayment(%v, %v) error: %v", pi.IncomingAmountMsat, pi.OutgoingAmountMsat, err)
		return nil, fmt.Errorf("checkPayment(%v, %v) error: %v", pi.IncomingAmountMsat, pi.OutgoingAmountMsat, err)
	}
	err = registerPayment(pi.Destination, pi.PaymentHash, pi.PaymentSecret, pi.IncomingAmountMsat, pi.OutgoingAmountMsat)
	if err != nil {
		log.Printf("RegisterPayment() error: %v", err)
		return nil, fmt.Errorf("RegisterPayment() error: %w", err)
	}
	return &lspdrpc.RegisterPaymentReply{}, nil
}

func (s *server_c) OpenChannel(ctx context.Context, in *lspdrpc.OpenChannelRequest) (*lspdrpc.OpenChannelReply, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *server_c) CheckChannels(ctx context.Context, in *lspdrpc.Encrypted) (*lspdrpc.Encrypted, error) {

	return nil, fmt.Errorf("not implemented")
}

//C-lightning plugin functions
func StartPlugin() {
	//c-lightning plugin initiate
	plugin := glightning.NewPlugin(onInit)
	plugin.RegisterHooks(&glightning.Hooks{
		HtlcAccepted: OnHtlcAccepted,
	})

	err := plugin.Start(os.Stdin, os.Stdout)
	if err != nil {
		log.Printf("start plugin error: %v", err)
	}
}
func onInit(plugin *glightning.Plugin, options map[string]glightning.Option, config *glightning.Config) {
	log.Printf("successfully init'd! %s\n", config.RpcFile)
}

func clnOpenChannel(client *glightning.Lightning, paymentHash, destination []byte, incomingAmountMsat int64) ([]byte, uint32, error) {
	capacity := incomingAmountMsat/1000 + additionalChannelCapacity
	if capacity == publicChannelAmount {
		capacity++
	}
	channelPoint, err := client.FundChannel(hex.EncodeToString(destination), glightning.NewSat(int(capacity)))

	if err != nil {
		log.Printf("client.OpenChannelSync(%x, %v) error: %v", destination, capacity, err)
		return nil, 0, err
	}
	outputin, err := strconv.Atoi(channelPoint.FundingTxId)
	if err != nil {
		log.Printf("strconv.Atoi(channelPoint.FundingTxId) error: %v", err)
	}
	err = setFundingTx(paymentHash, []byte(channelPoint.FundingTx), outputin)
	return []byte(channelPoint.FundingTx), uint32(outputin), err
}
func OnHtlcAccepted(event *glightning.HtlcAcceptedEvent) (*glightning.HtlcAcceptedResponse, error) {
	log.Printf("htlc_accepted called\n")

	onion := event.Onion
	log.Printf("has perhop? %t", onion.PerHop != nil)
	log.Printf("type is %s", onion.Type)

	var on string
	if onion.PaymentSecret == "" {
		on = ""
	} else {
		on = "not "
	}
	log.Printf("payment secret is %sempty", on)

	if onion.TotalMilliSatoshi == "" {
		on = "empty"
	} else {
		on = onion.TotalMilliSatoshi
	}
	log.Printf("amount is %s", on)
	//cancellableCtx, _ := context.WithCancel(context.Background())
	//clientCtx := metadata.AppendToOutgoingContext(cancellableCtx)
	client := glightning.NewLightning()
	fmt.Printf("htlc: %v\nchanID: %v\nincoming amount: %v\noutgoing amount: %v\nincomin expiry: %v\noutgoing expiry: %v\npaymentHash: %x\nonionBlob: %x\n\n",
		event.Htlc,
		onion.ShortChannelId,
		onion.TotalMilliSatoshi,
		onion.ForwardAmount,
		event.Htlc.CltvExpiryRelative,
		event.Htlc.CltvExpiry,
		event.Htlc.PaymentHash,
		onion,
	)

	paymentHash, paymentSecret, destination, incomingAmountMsat, outgoingAmountMsat, fundingTxID, fundingTxOutnum, err := paymentInfo([]byte(event.Htlc.PaymentHash))
	if err != nil {
		log.Printf("paymentInfo(%x) error: %v", event.Htlc.PaymentHash, err)
	}
	log.Printf("paymentHash:%x\npaymentSecret:%x\ndestination:%x\nincomingAmountMsat:%v\noutgoingAmountMsat:%v\n\n",
		paymentHash, paymentSecret, destination, incomingAmountMsat, outgoingAmountMsat)
	if paymentSecret != nil {

		if fundingTxID == nil {
			if bytes.Compare(paymentHash, []byte(event.Htlc.PaymentHash)) == 0 {
				//need to implement open channel with openchannel hook

				fundingTxID, fundingTxOutnum, err = clnOpenChannel(client, []byte(event.Htlc.PaymentHash), destination, incomingAmountMsat)
				log.Printf("openclnOpenChannelChannel(%x, %v) err: %v", destination, incomingAmountMsat, err)
				if err != nil {
					log.Printf(" clnOpenChannel error: %v", err)
				}
			}

		}

	}

	return event.Continue(), nil
}

func run_cln() {
	var wg sync.WaitGroup
	//c-lightning plugin started
	wg.Add(1)
	go StartPlugin()

	err := pgConnect()
	if err != nil {
		log.Fatalf("pgConnect() error: %v", err)
	}
	privateKeyBytes, err := hex.DecodeString(os.Getenv("LSPD_PRIVATE_KEY_CLN"))
	if err != nil {
		log.Fatalf("hex.DecodeString(os.Getenv(\"LSPD_PRIVATE_KEY_CLN\")=%v) error: %v", os.Getenv("LSPD_PRIVATE_KEY_CLN"), err)
	}
	cln_privateKey, cln_publicKey = btcec.PrivKeyFromBytes(btcec.S256(), privateKeyBytes)

	//grpc server for clightning
	address := os.Getenv("LISTEN_ADDRESS_CLN")
	var lis net.Listener

	lis, err = net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer(
		grpc_middleware.WithUnaryServerChain(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				for _, auth := range md.Get("authorization") {
					if auth == "Bearer "+os.Getenv("CLN_TOKEN") {
						return handler(ctx, req)
					}
				}
			}
			return nil, status.Errorf(codes.PermissionDenied, "Not authorized")
		}),
	)
	lspdrpc.RegisterChannelOpenerServer(s, &server_c{})
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}

	wg.Wait() //wait for greenlight intercept
}
