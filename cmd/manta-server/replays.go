package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dotabuff/manta/cmd/internal/opendota"
)

// ReplayResolver turns a match id into a replay download URL.
type ReplayResolver interface {
	Resolve(ctx context.Context, matchID int64, progress func(string)) (string, error)
}

// ResolverFunc adapts a function to ReplayResolver.
type ResolverFunc func(ctx context.Context, matchID int64, progress func(string)) (string, error)

func (f ResolverFunc) Resolve(ctx context.Context, matchID int64, progress func(string)) (string, error) {
	return f(ctx, matchID, progress)
}

// ErrReplayUnavailable means the replay cannot be located or fetched.
var ErrReplayUnavailable = errors.New("replay unavailable")

// openDotaResolver locates replays through OpenDota. When OpenDota has not
// seen the match it asks OpenDota to fetch it and polls until the replay salt
// appears or the wait runs out.
type openDotaResolver struct {
	client      *opendota.Client
	requestWait time.Duration
	poll        time.Duration
}

func (r *openDotaResolver) Resolve(ctx context.Context, matchID int64, progress func(string)) (string, error) {
	rep, ok, err := r.client.FindReplay(matchID)
	if err != nil {
		return "", fmt.Errorf("looking up replay on OpenDota: %w", err)
	}
	if ok {
		return rep.URL(), nil
	}

	progress("OpenDota has not seen this match yet; asking it to fetch the match details")
	if _, err := r.client.RequestParse(matchID); err != nil {
		return "", fmt.Errorf("%w: OpenDota could not be asked to fetch the match: %v", ErrReplayUnavailable, err)
	}
	deadline := time.Now().Add(r.requestWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(r.poll):
		}
		progress(fmt.Sprintf("waiting for OpenDota to locate the replay (%s left)", time.Until(deadline).Round(time.Second)))
		rep, ok, err := r.client.FindReplay(matchID)
		if err != nil {
			return "", fmt.Errorf("looking up replay on OpenDota: %w", err)
		}
		if ok {
			return rep.URL(), nil
		}
	}
	return "", fmt.Errorf("%w: OpenDota has no replay information for match %d yet; try again in a few minutes", ErrReplayUnavailable, matchID)
}

// Downloader fetches replay files with a size cap and progress reporting.
type Downloader struct {
	HTTP     *http.Client
	MaxBytes int64
}

// Download streams url to dst (via a temp file), reporting bytes so far and
// the total when known.
func (d *Downloader) Download(ctx context.Context, url, dst string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "manta-server (github.com/dotabuff/manta)")
	res, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReplayUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: Valve no longer serves this replay (HTTP 404); replays expire after a few weeks", ErrReplayUnavailable)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d from the replay server", ErrReplayUnavailable, res.StatusCode)
	}
	if d.MaxBytes > 0 && res.ContentLength > d.MaxBytes {
		return fmt.Errorf("replay is %d MB, above the %d MB limit", res.ContentLength>>20, d.MaxBytes>>20)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	var done int64
	buf := make([]byte, 256<<10)
	body := res.Body
	if d.MaxBytes > 0 {
		body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(res.Body, d.MaxBytes+1), res.Body}
	}
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return werr
			}
			done += int64(n)
			if d.MaxBytes > 0 && done > d.MaxBytes {
				tmp.Close()
				return fmt.Errorf("replay exceeds the %d MB limit", d.MaxBytes>>20)
			}
			progress(done, res.ContentLength)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return fmt.Errorf("%w: %v", ErrReplayUnavailable, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
