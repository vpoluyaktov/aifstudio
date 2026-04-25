package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"storycloud/internal/store"
)

const transcriptContentType = "text/plain; charset=utf-8"

// transcriptBuffer accumulates game output and flushes to GCS on session end.
type transcriptBuffer struct {
	runID string
	sb    strings.Builder
}

func newTranscriptBuffer(runID string) *transcriptBuffer {
	return &transcriptBuffer{runID: runID}
}

func (tb *transcriptBuffer) write(text string) {
	tb.sb.WriteString(text)
}

// flush uploads the accumulated transcript to GCS and returns the GCS path.
// New runs use sessions/<runId>/transcript.txt (§A.4.1).
func (tb *transcriptBuffer) flush(ctx context.Context, st store.Store) (string, error) {
	if tb.sb.Len() == 0 {
		return "", nil
	}
	path := fmt.Sprintf("sessions/%s/transcript.txt", tb.runID)
	content := tb.sb.String()
	r := bytes.NewReader([]byte(content))
	if err := st.UploadBlob(ctx, path, transcriptContentType, r); err != nil {
		slog.Error("transcript flush failed", "runId", tb.runID, "err", err)
		return "", err
	}
	return path, nil
}
