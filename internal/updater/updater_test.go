package updater

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/config"
)

func TestRuntimeURLsUseHbashtonRepository(t *testing.T) {
	t.Parallel()

	urls := map[string]string{
		"repository": repositoryURL,
		"releases":   releasesAPIURL,
		"installer":  installScriptBaseURL,
		"release":    releaseURL("v1.2.3"),
	}
	for name, runtimeURL := range urls {
		if !strings.Contains(runtimeURL, "hbashton/VIIPER") {
			t.Errorf("%s URL %q does not target hbashton/VIIPER", name, runtimeURL)
		}
		if strings.Contains(strings.ToLower(runtimeURL), "alia5") {
			t.Errorf("%s URL %q retains the Alia5 repository", name, runtimeURL)
		}
	}
}

func TestLocalTestBuildSkipsReleaseNetworkAndParseErrors(t *testing.T) {
	previousClient := client
	t.Cleanup(func() { client = previousClient })
	transportCalled := make(chan struct{}, 1)
	client = &http.Client{
		Timeout: time.Second,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			transportCalled <- struct{}{}
			return nil, nil
		}),
	}
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	var records strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&records, nil)))

	CheckUpdate("0.1.0-local-test", config.UpdateNotifyStable)
	select {
	case <-transportCalled:
		t.Fatal("local-test update check reached the network")
	default:
	}
	if records.Len() != 0 {
		t.Fatalf("local-test update log=%q", records.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestReleaseURLEscapesTag(t *testing.T) {
	t.Parallel()

	const want = "https://github.com/hbashton/VIIPER/releases/tag/release%2Fcandidate"
	if got := releaseURL("release/candidate"); got != want {
		t.Fatalf("releaseURL() = %q, want %q", got, want)
	}
}
