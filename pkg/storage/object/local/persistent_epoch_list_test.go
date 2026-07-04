package local_test

import (
	"testing"

	object_local "bonanza.build/pkg/storage/object/local"

	"github.com/buildbarn/bb-storage/pkg/random"
	"github.com/stretchr/testify/require"
)

func TestPersistentEpochListDiscardDuringWrite(t *testing.T) {
	// Regression test: if DiscardUpToLocation() discards all
	// epochs while a write is in progress (e.g., because a
	// corrupted object was detected by a concurrent read),
	// FinalizeWriteUpToLocation() used to leave the EpochList
	// without a current epoch, causing GetCurrentEpochState() to
	// panic.
	el := object_local.NewPersistentEpochList(
		/* maximumLocationSpan = */ 1000,
		random.NewFastSingleThreadedGenerator(),
		/* minimumEpochID = */ 1,
		/* minimumLocation = */ 0,
		/* epochs = */ nil,
	)

	// Write an object covering locations [0, 100).
	require.NoError(t, el.FinalizeWriteUpToLocation(100))
	_, epochID1 := el.GetCurrentEpochState()

	// Detect corruption of an object covering locations [100,
	// 500), discarding all epochs.
	el.DiscardUpToLocation(500)

	// Finalizing a write that ended before the discarded location
	// must still leave the EpochList with a usable current epoch.
	require.NoError(t, el.FinalizeWriteUpToLocation(200))
	epochState, epochID2 := el.GetCurrentEpochState()
	require.NotEqual(t, epochID1, epochID2)
	require.Equal(t, uint64(500), epochState.MinimumLocation)
	require.Equal(t, uint64(500), epochState.MaximumLocation)
}
