package registry

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const maxDevinModelsSize = 8 << 20

var devinModelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/devin_models.json",
	"https://models.router-for.me/devin_models.json",
}

var devinModelsUpdaterOnce sync.Once

// StartDevinModelsUpdater starts a background updater that fetches the
// Devin model catalog immediately and refreshes it every 3 hours.
// Safe to call multiple times; only one updater runs.
func StartDevinModelsUpdater(ctx context.Context) {
	devinModelsUpdaterOnce.Do(func() {
		go runDevinModelsUpdater(ctx)
	})
}

func runDevinModelsUpdater(ctx context.Context) {
	tryRefreshDevinModels(ctx, "startup Devin model refresh")

	ticker := time.NewTicker(modelsRefreshInterval)
	defer ticker.Stop()
	log.Infof("periodic Devin model refresh started (interval=%s)", modelsRefreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryRefreshDevinModels(ctx, "periodic Devin model refresh")
		}
	}
}

func tryRefreshDevinModels(ctx context.Context, label string) {
	data, sourceURL := fetchDevinModelsFromRemote(ctx)
	if data == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data (embedded or cached fallback)", label)
		return
	}

	changed, errLoad := loadDevinModelsFromBytes(data, sourceURL)
	if errLoad != nil {
		log.Warnf("%s: fetched catalog rejected, keeping current data: %v", label, errLoad)
		return
	}
	if !changed {
		log.Infof("%s completed from %s, no changes detected", label, sourceURL)
		return
	}
	log.Infof("%s completed from %s, catalog updated", label, sourceURL)
	notifyModelRefresh([]string{"devin"})
}

func fetchDevinModelsFromRemote(ctx context.Context) ([]byte, string) {
	client := &http.Client{Timeout: modelsFetchTimeout}
	for _, sourceURL := range devinModelsURLs {
		reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
		req, errRequest := http.NewRequestWithContext(reqCtx, http.MethodGet, sourceURL, nil)
		if errRequest != nil {
			cancel()
			log.Warnf("devin models updater: invalid request for %s: %v", sourceURL, errRequest)
			continue
		}

		resp, errDo := client.Do(req)
		if errDo != nil {
			cancel()
			log.Warnf("devin models updater: fetch failed from %s: %v", sourceURL, errDo)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("devin models updater: response body close error: %v", errClose)
			}
			cancel()
			log.Warnf("devin models updater: unexpected status %d from %s", resp.StatusCode, sourceURL)
			continue
		}

		body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxDevinModelsSize))
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("devin models updater: response body close error: %v", errClose)
		}
		cancel()
		if errRead != nil {
			log.Warnf("devin models updater: read failed from %s: %v", sourceURL, errRead)
			continue
		}
		if _, errValidate := ValidateDevinModelsJSON(body); errValidate != nil {
			log.Warnf("devin models updater: invalid catalog from %s: %v", sourceURL, errValidate)
			continue
		}

		return body, sourceURL
	}
	return nil, ""
}
