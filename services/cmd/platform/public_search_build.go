package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/search"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

type publicSearchTipSampler func(context.Context) (search.PublicOffsets, error)

func publicSearchBuild(ctx context.Context, role string, args []string, input io.Reader, output io.Writer) error {
	if role != publicSearchWorker || len(args) == 0 {
		return errors.New("use -role public-search-workflow-worker search-build sample-replay-start|begin|load|seal|status|replay-status|current|publish")
	}
	brokers := strings.Split(os.Getenv("MIAOQIA_KAFKA_BROKERS"), ",")
	sample := func(ctx context.Context) (search.PublicOffsets, error) { return samplePublicSearchTip(ctx, brokers) }
	if args[0] == "sample-replay-start" {
		return executeSearchBuild(ctx, nil, args, input, output, sample)
	}
	dsn, err := publicSearchDSN("DB_DSN", publicSearchApp)
	if err != nil {
		return err
	}
	database, err := openDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	projector, err := publicSearchProjector(database, func(key string) string { return os.Getenv(publicSearchPrefix + key) })
	if err != nil {
		return err
	}
	return executeSearchBuild(reqctx.Service(ctx, publicSearchWorker), projector, args, input, output, sample)
}

func executeSearchBuild(ctx context.Context, projector *search.PublicProjector, args []string, input io.Reader, output io.Writer, sampleTip publicSearchTipSampler) error {
	command, params := args[0], args[1:]
	const limit = 1 << 20
	encode := json.NewEncoder(output).Encode
	switch command {
	case "sample-replay-start":
		if len(params) != 0 {
			return errors.New("sample-replay-start takes no arguments")
		}
		start, err := sampleTip(ctx)
		if err != nil {
			return err
		}
		return encode(start)
	case "begin":
		var basis search.PublicBuildBasis
		if len(params) != 0 || decodeStrictJSON(io.LimitReader(input, limit+1), &basis) != nil {
			return errors.New("begin requires a snapshot basis on stdin")
		}
		generation, err := projector.BeginBuild(ctx, basis)
		if err != nil {
			return err
		}
		return encode(map[string]string{"generation": generation})
	case "load", "seal":
		if len(params) != 2 {
			return errors.New("load and seal require a generation and snapshot ID")
		}
		raw, err := io.ReadAll(io.LimitReader(input, limit+1))
		if err != nil || len(raw) > limit {
			return errors.New("owner snapshot page exceeds its read boundary")
		}
		page, err := decodePublicMutationPage(bytes.NewReader(raw), "")
		if err != nil || page.NextAfter != "" {
			return errors.New("load and seal require a confirmed owner snapshot page on stdin")
		}
		return projector.LoadSnapshot(ctx, params[0], params[1], page.Mutations, command == "seal")
	case "status":
		if len(params) != 1 {
			return errors.New("status requires a generation")
		}
		status, err := projector.BuildStatus(ctx, params[0])
		if err != nil {
			return err
		}
		return encode(status)
	case "replay-status":
		var barrier search.PublicOffsets
		if len(params) != 1 || decodeStrictJSON(io.LimitReader(input, limit+1), &barrier) != nil {
			return errors.New("replay-status requires a generation and replay barrier on stdin")
		}
		status, err := projector.ReplayStatus(ctx, params[0], barrier)
		if err != nil {
			return err
		}
		return encode(status)
	case "current":
		if len(params) != 0 {
			return errors.New("current takes no arguments")
		}
		publication, err := projector.CurrentPublication(ctx)
		if errors.Is(err, search.ErrPublicIndexUnpublished) {
			return encode(nil)
		}
		if err != nil {
			return err
		}
		return encode(publication)
	case "publish":
		var approval search.PublicBuildAssessment
		if len(params) != 2 || decodeStrictJSON(io.LimitReader(input, limit+1), &approval) != nil {
			return errors.New("publish requires a generation, expected activation and build assessment on stdin")
		}
		tip, err := sampleTip(ctx)
		if err != nil {
			return err
		}
		publication, err := projector.Publish(ctx, params[0], params[1], approval, tip)
		if err != nil {
			return err
		}
		return encode(publication)
	default:
		return errors.New("unknown search-build command")
	}
}

func samplePublicSearchTip(ctx context.Context, brokers []string) (search.PublicOffsets, error) {
	if len(brokers) == 0 || strings.TrimSpace(brokers[0]) == "" {
		return nil, errors.New("public search Kafka sampling requires MIAOQIA_KAFKA_BROKERS")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	transport, err := transportsecurity.KafkaOptions()
	if err != nil {
		return nil, err
	}
	client, err := kgo.NewClient(append(transport, kgo.SeedBrokers(brokers...))...)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	topics := search.PublicEventTopics()
	listed, err := kadm.NewClient(client).ListEndOffsets(ctx, topics...)
	if err != nil {
		return nil, fmt.Errorf("public search sample Kafka end offsets: %w", err)
	}
	if err := listed.Error(); err != nil {
		return nil, fmt.Errorf("public search sample Kafka end offsets: %w", err)
	}
	tip := make(search.PublicOffsets, len(topics))
	for _, topic := range topics {
		if len(listed[topic]) == 0 {
			return nil, fmt.Errorf("public search Kafka topic %s has no sampled partitions", topic)
		}
		tip[topic] = make(map[int32]int64, len(listed[topic]))
		for partition, offset := range listed[topic] {
			if partition < 0 || offset.Offset < 0 {
				return nil, fmt.Errorf("public search invalid Kafka end offset for %s[%d]", topic, partition)
			}
			tip[topic][partition] = offset.Offset
		}
	}
	return tip, nil
}
