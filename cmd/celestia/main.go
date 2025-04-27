package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/celestiaorg/celestia-app/v3/app"
	"github.com/celestiaorg/celestia-app/v3/pkg/appconsts"
	"github.com/celestiaorg/celestia-app/v3/pkg/da"
	"github.com/celestiaorg/celestia-app/v3/pkg/wrapper"
	"github.com/celestiaorg/celestia-node/share"
	libsquare "github.com/celestiaorg/go-square/v2"
	libshare "github.com/celestiaorg/go-square/v2/share"
	"github.com/celestiaorg/nmt"
	"github.com/celestiaorg/rsmt2d"
	"github.com/gogo/protobuf/proto"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	coregrpc "github.com/tendermint/tendermint/rpc/grpc"
	"github.com/tendermint/tendermint/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type SignedBlock struct {
	Header       *types.Header       `json:"header"`
	Commit       *types.Commit       `json:"commit"`
	Data         *types.Data         `json:"data"`
	ValidatorSet *types.ValidatorSet `json:"validator_set"`
}

type CoreAccessor struct {
	ctx    context.Context
	client coregrpc.BlockAPIClient
}

// ExtendedHeader represents a wrapped "raw" header that includes
// information necessary for Celestia Nodes to be notified of new
// block headers and perform Data Availability Sampling.
type ExtendedHeader struct {
	types.Header `json:"header"`
	Commit       *types.Commit              `json:"commit"`
	ValidatorSet *types.ValidatorSet        `json:"validator_set"`
	DAH          *da.DataAvailabilityHeader `json:"dah"`
}

func NewCoreAccessor(ip string) (*CoreAccessor, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	conn, err := grpc.NewClient(ip, opts...)
	if err != nil {
		return nil, err
	}
	ctx := context.WithoutCancel(context.Background())

	client := coregrpc.NewBlockAPIClient(conn)

	return &CoreAccessor{ctx, client}, nil
}

func (c CoreAccessor) getSignedBlock(h string) (*SignedBlock, error) {
	// Third argument is block height
	height, err := strconv.Atoi(h)
	if err != nil {
		return nil, err
	}

	stream, err := c.client.BlockByHeight(c.ctx, &coregrpc.BlockByHeightRequest{Height: int64(height)})
	if err != nil {
		return nil, err
	}
	block, err := receiveBlockByHeight(stream)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func receiveBlockByHeight(streamer coregrpc.BlockAPI_BlockByHeightClient) (
	*SignedBlock,
	error,
) {
	parts := make([]*tmproto.Part, 0)

	// receive the first part to get the block meta, commit, and validator set
	firstPart, err := streamer.Recv()
	if err != nil {
		return nil, err
	}
	commit, err := types.CommitFromProto(firstPart.Commit)
	if err != nil {
		return nil, err
	}
	validatorSet, err := types.ValidatorSetFromProto(firstPart.ValidatorSet)
	if err != nil {
		return nil, err
	}
	parts = append(parts, firstPart.BlockPart)

	// receive the rest of the block
	isLast := firstPart.IsLast
	for !isLast {
		resp, err := streamer.Recv()
		if err != nil {
			return nil, err
		}
		parts = append(parts, resp.BlockPart)
		isLast = resp.IsLast
	}
	block, err := partsToBlock(parts)
	if err != nil {
		return nil, err
	}
	return &SignedBlock{
		Header:       &block.Header,
		Commit:       commit,
		Data:         &block.Data,
		ValidatorSet: validatorSet,
	}, nil
}

// partsToBlock takes a slice of parts and generates the corresponding block.
// It empties the slice to optimize the memory usage.
func partsToBlock(parts []*tmproto.Part) (*types.Block, error) {
	partSet := types.NewPartSetFromHeader(types.PartSetHeader{
		Total: uint32(len(parts)),
	})
	for _, part := range parts {
		ok, err := partSet.AddPartWithoutProof(&types.Part{Index: part.Index, Bytes: part.Bytes})
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, err
		}
	}
	pbb := new(tmproto.Block)
	bz, err := io.ReadAll(partSet.GetReader())
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(bz, pbb)
	if err != nil {
		return nil, err
	}
	block, err := types.BlockFromProto(pbb)
	if err != nil {
		return nil, err
	}
	return block, nil
}

// extendBlock extends the given block data, returning the resulting
// ExtendedDataSquare (EDS). If there are no transactions in the block,
// nil is returned in place of the eds.
func extendBlock(data *types.Data, appVersion uint64, options ...nmt.Option) (*rsmt2d.ExtendedDataSquare, error) {
	if app.IsEmptyBlockRef(data, appVersion) {
		return share.EmptyEDS(), nil
	}

	// Construct the data square from the block's transactions
	square, err := libsquare.Construct(
		data.Txs.ToSliceOfBytes(),
		appconsts.SquareSizeUpperBound(appVersion),
		appconsts.SubtreeRootThreshold(appVersion),
	)
	if err != nil {
		return nil, err
	}
	return extendShares(libshare.ToBytes(square), options...)
}

func extendShares(s [][]byte, options ...nmt.Option) (*rsmt2d.ExtendedDataSquare, error) {
	// Check that the length of the square is a power of 2.
	if !libsquare.IsPowerOfTwo(len(s)) {
		return nil, fmt.Errorf("number of shares is not a power of 2: got %d", len(s))
	}
	// here we construct a tree
	// Note: uses the nmt wrapper to construct the tree.
	squareSize := libsquare.Size(len(s))
	return rsmt2d.ComputeExtendedDataSquare(s,
		appconsts.DefaultCodec(),
		wrapper.NewConstructor(uint64(squareSize),
			options...))
}

// makeExtendedHeader assembles new ExtendedHeader.
func makeExtendedHeader(
	h *types.Header,
	comm *types.Commit,
	vals *types.ValidatorSet,
	eds *rsmt2d.ExtendedDataSquare,
) (*ExtendedHeader, error) {
	var (
		dah da.DataAvailabilityHeader
		err error
	)
	switch eds {
	case nil:
		dah = da.MinDataAvailabilityHeader()
	default:
		dah, err = da.NewDataAvailabilityHeader(eds)
		if err != nil {
			return nil, err
		}
	}

	eh := &ExtendedHeader{
		Header:       *h,
		DAH:          &dah,
		Commit:       comm,
		ValidatorSet: vals,
	}
	return eh, nil
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(0)
	}

	// First argument is the core address
	coreAccessor, err := NewCoreAccessor(args[0])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Second argument is command
	switch args[1] {
	case "eds":
		fmt.Println("eds")
		// Third argument is block height
		block, err := coreAccessor.getSignedBlock(args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		eds, err := extendBlock(block.Data, block.Header.Version.App)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		// create extended header
		eh, err := makeExtendedHeader(block.Header, block.Commit, block.ValidatorSet, eds)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(eh)
	case "share":
		fmt.Println("share")
		// Third argument is block height
		block, err := coreAccessor.getSignedBlock(args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		eds, err := extendBlock(block.Data, block.Header.Version.App)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		// Fourth and fifth arguments are indices
		r, err := strconv.Atoi(args[3])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		c, err := strconv.Atoi(args[4])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(eds.GetCell(uint(r), uint(c)))
	case "blob":
		fmt.Println("blob")
		// TODO
	case "block":
		fmt.Println("block")
		// Third argument is block height
		block, err := coreAccessor.getSignedBlock(args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Println(block)
	default:
		os.Exit(0)
	}
	os.Exit(0)
}
