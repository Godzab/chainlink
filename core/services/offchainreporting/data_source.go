package offchainreporting

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/smartcontractkit/chainlink/core/bridges"

	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/core/utils"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting/types"
)

// dataSource is an abstraction over the process of initiating a pipeline run
// and capturing the result. Additionally, it converts the result to an
// ocrtypes.Observation (*big.Int), as expected by the offchain reporting library.
type dataSource struct {
	pipelineRunner pipeline.Runner
	jobSpec        job.Job
	spec           pipeline.Spec
	ocrLogger      logger.Logger
	runResults     chan<- pipeline.Run

	current bridges.BridgeMetaData
	mu      sync.RWMutex
}

var _ ocrtypes.DataSource = (*dataSource)(nil)

func (ds *dataSource) updateAnswer(a *big.Int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.current = bridges.BridgeMetaData{
		LatestAnswer: a,
		UpdatedAt:    big.NewInt(time.Now().Unix()),
	}
}

func (ds *dataSource) currentAnswer() (*big.Int, *big.Int) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.current.LatestAnswer, ds.current.UpdatedAt
}

// The context passed in here has a timeout of (ObservationTimeout + ObservationGracePeriod).
// Upon context cancellation, its expected that we return any usable values within ObservationGracePeriod.
func (ds *dataSource) Observe(ctx context.Context) (ocrtypes.Observation, error) {
	var observation ocrtypes.Observation
	md, err := bridges.MarshalBridgeMetaData(ds.currentAnswer())
	if err != nil {
		ds.ocrLogger.Warnw("unable to attach metadata for run", "err", err)
	}

	vars := pipeline.NewVarsFrom(map[string]interface{}{
		"jobSpec": map[string]interface{}{
			"databaseID":    ds.jobSpec.ID,
			"externalJobID": ds.jobSpec.ExternalJobID,
			"name":          ds.jobSpec.Name.ValueOrZero(),
		},
		"jobRun": map[string]interface{}{
			"meta": md,
		},
	})

	run, trrs, err := ds.pipelineRunner.ExecuteRun(ctx, ds.spec, vars, ds.ocrLogger)
	if err != nil {
		return observation, errors.Wrapf(err, "error executing run for spec ID %v", ds.spec.ID)
	}
	finalResult := trrs.FinalResult(ds.ocrLogger)

	// Do the database write in a non-blocking fashion
	// so we can return the observation results immediately.
	// This is helpful in the case of a blocking API call, where
	// we reach the passed in context deadline and we want to
	// immediately return any result we have and do not want to have
	// a db write block that.
	select {
	case ds.runResults <- run:
	default:
		return nil, errors.Errorf("unable to enqueue run save for job ID %v, buffer full", ds.spec.JobID)
	}

	result, err := finalResult.SingularResult()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting singular result for job ID %v", ds.spec.JobID)
	}

	if result.Error != nil {
		return nil, result.Error
	}

	asDecimal, err := utils.ToDecimal(result.Value)
	if err != nil {
		return nil, errors.Wrap(err, "cannot convert observation to decimal")
	}
	ds.updateAnswer(asDecimal.BigInt())
	return asDecimal.BigInt(), nil
}
