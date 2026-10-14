package llm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/llm"
)

type fakeClient struct {
	text  string
	usage llm.Usage
	err   error
}

func (f fakeClient) Complete(context.Context, string, string) (string, llm.Usage, error) {
	return f.text, f.usage, f.err
}

type fakeRecorder struct {
	got []llm.CallRecord
	err error
}

func (f *fakeRecorder) RecordLLMCall(c llm.CallRecord) error {
	f.got = append(f.got, c)

	return f.err
}

func TestRecordingClient(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	cached := int64(7)
	errLength := errors.New("finish_reason=length")

	tests := []struct {
		name    string
		ctx     context.Context
		inner   fakeClient
		recErr  error
		want    llm.CallRecord
		wantErr []error
	}{
		{
			name:  "success records configured model, stage and usage",
			ctx:   llm.WithStage(context.Background(), "cluster"),
			inner: fakeClient{text: "ok", usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20, CachedPromptTokens: &cached, Model: "served-name"}},
			want:  llm.CallRecord{At: at, Stage: "cluster", Model: "configured", Prompt: 100, Completion: 20, Cached: &cached},
		},
		{
			name:    "an error carrying usage still records and returns the original error",
			ctx:     llm.WithStage(context.Background(), "match"),
			inner:   fakeClient{usage: llm.Usage{PromptTokens: 50, CompletionTokens: 9}, err: errLength},
			want:    llm.CallRecord{At: at, Stage: "match", Model: "configured", Prompt: 50, Completion: 9},
			wantErr: []error{errLength},
		},
		{
			name:    "an error with no usage records zeros",
			ctx:     llm.WithStage(context.Background(), "match"),
			inner:   fakeClient{err: errLength},
			want:    llm.CallRecord{At: at, Stage: "match", Model: "configured"},
			wantErr: []error{errLength},
		},
		{
			name:  "unset stage records unstaged",
			ctx:   context.Background(),
			inner: fakeClient{text: "ok"},
			want:  llm.CallRecord{At: at, Stage: llm.UnstagedStage, Model: "configured"},
		},
		{
			name:   "a recorder error is returned wrapped with the text",
			ctx:    context.Background(),
			inner:  fakeClient{text: "ok"},
			recErr: errors.New("disk full"),
			want:   llm.CallRecord{At: at, Stage: llm.UnstagedStage, Model: "configured"},
		},
		{
			name:   "a recorder error joins the inner error",
			ctx:    context.Background(),
			inner:  fakeClient{err: errLength},
			recErr: errors.New("disk full"),
			want:   llm.CallRecord{At: at, Stage: llm.UnstagedStage, Model: "configured"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &fakeRecorder{err: tt.recErr}
			c := llm.NewRecordingClient(tt.inner, rec, "configured", now)

			text, usage, err := c.Complete(tt.ctx, "sys", "user")

			assert.Equal(t, tt.inner.text, text)
			assert.Equal(t, tt.inner.usage, usage)
			require.Len(t, rec.got, 1)
			if diff := cmp.Diff(tt.want, rec.got[0]); diff != "" {
				t.Errorf("record mismatch (-want +got):\n%s", diff)
			}

			switch {
			case tt.recErr != nil:
				require.ErrorIs(t, err, tt.recErr)
				require.ErrorContains(t, err, "recording llm call")
				if tt.inner.err != nil {
					require.ErrorIs(t, err, tt.inner.err)
				}
			case len(tt.wantErr) > 0:
				for _, w := range tt.wantErr {
					require.ErrorIs(t, err, w)
				}
			default:
				require.NoError(t, err)
			}
		})
	}
}
