package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/google/uuid"
	willf_bloom "github.com/willf/bloom"

	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/encoding"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

type bloomCmd struct {
	TenantID       string  `arg:"" help:"tenant-id within the bucket"`
	BlockID        string  `arg:"" help:"block ID to list"`
	BloomFP        float64 `arg:"" help:"bloom filter false positive rate (use prod settings!)"`
	BloomShardSize int     `arg:"" help:"bloom filter shard size (use prod settings!)"`
	backendOptions
}

type forEachRecord func(id common.ID) error

func ReplayBlockAndDoForEachRecord(meta *backend.BlockMeta, filepath string, forEach forEachRecord) error {
	v, err := encoding.FromVersion(meta.Version)
	if err != nil {
		return err
	}

	// replay file to extract records
	f, err := os.OpenFile(filepath, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}

	dataReader, err := v.NewDataReader(backend.NewContextReaderWithAllReader(f), meta.Encoding)
	if err != nil {
		return fmt.Errorf("error creating data reader: %w", err)
	}
	defer dataReader.Close()

	var buffer []byte
	objectRW := v.NewObjectReaderWriter()
	for {
		buffer, _, err := dataReader.NextPage(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading page from datareader: %w", err)
		}

		iter := encoding.NewIterator(bytes.NewReader(buffer), objectRW)
		var iterErr error
		for {
			var id common.ID
			id, _, iterErr = iter.Next(context.TODO())
			if iterErr != nil {
				break
			}
			err := forEach(id)
			if err != nil {
				return fmt.Errorf("error adding to bloom filter: %w", err)
			}
		}

		if iterErr != io.EOF {
			return iterErr
		}
	}

	return nil
}

func (cmd *bloomCmd) Run(ctx *globalOptions) error {
	blockID, err := uuid.Parse(cmd.BlockID)
	if err != nil {
		return err
	}

	r, w, _, err := loadBackend(&cmd.backendOptions, ctx)
	if err != nil {
		return err
	}

	meta, err := r.BlockMeta(context.TODO(), blockID, cmd.TenantID)
	if err != nil {
		return err
	}

	// replay file and add records to bloom filter
	bloom := common.NewBloom(cmd.BloomFP, uint(cmd.BloomShardSize), uint(meta.TotalObjects))
	if bloom.GetShardCount() != int(meta.BloomShardCount) {
		err := fmt.Errorf("shards in generated bloom filter do not match block meta, please use prod settings for bloom shard size and FP")
		fmt.Println(err.Error())
		return err
	}

	addToBloom := func(id common.ID) error {
		bloom.Add(id)
		return nil
	}

	err = ReplayBlockAndDoForEachRecord(meta, cmd.backendOptions.Bucket+cmd.TenantID+"/"+cmd.BlockID+"/"+dataFilename, addToBloom)
	if err != nil {
		fmt.Println("error replaying block", err)
		return err
	}

	// write to the local backend
	bloomBytes, err := bloom.Marshal()
	if err != nil {
		fmt.Println("error marshalling bloom filter")
		return err
	}

	for i := 0; i < len(bloomBytes); i++ {
		err = w.Write(context.TODO(), bloomFilePrefix+strconv.Itoa(i), blockID, cmd.TenantID, bloomBytes[i], false)
		if err != nil {
			fmt.Println("error writing bloom filter to backend", err)
			return err
		}
	}

	fmt.Println("bloom written to backend successfully")

	// verify generated bloom
	shardedBloomFilter := make([]*willf_bloom.BloomFilter, meta.BloomShardCount)
	for i := 0; i < int(meta.BloomShardCount); i++ {
		bloomBytes, err := r.Read(context.TODO(), bloomFilePrefix+strconv.Itoa(i), blockID, cmd.TenantID, false)
		if err != nil {
			fmt.Println("error reading bloom from backend")
			return nil
		}
		shardedBloomFilter[i] = &willf_bloom.BloomFilter{}
		_, err = shardedBloomFilter[i].ReadFrom(bytes.NewReader(bloomBytes))
		if err != nil {
			fmt.Println("error parsing bloom")
			return nil
		}
	}

	testBloom := func(id common.ID) error {
		key := common.ShardKeyForTraceID(id, int(meta.BloomShardCount))
		if !shardedBloomFilter[key].Test(id) {
			return fmt.Errorf("id not added to bloom, filter is likely corrupt")
		}
		return nil
	}
	err = ReplayBlockAndDoForEachRecord(meta, cmd.backendOptions.Bucket+cmd.TenantID+"/"+cmd.BlockID+"/"+dataFilename, testBloom)
	if err != nil {
		fmt.Println("error replaying block", err)
		return err
	}

	fmt.Println("bloom filter verified")
	return nil
}
