package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

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
	case "share":
		fmt.Println("share")
		// Third argument is block height
		block, err := coreAccessor.getSignedBlock(args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(block)
	case "blob":
		fmt.Println("blob")
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
