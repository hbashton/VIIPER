package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
)

const (
	matrixSchema   = "viiper.controller-to-game.latency-priority-matrix/v2"
	evidenceSchema = "viiper.controller-to-game.latency-superiority-evidence/v1"
)

var (
	hashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	cycleIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type evidenceFile struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

type matrixRun struct {
	PriorityClass string       `json:"priority_class"`
	PriorityCycle int          `json:"priority_cycle"`
	CycleIndex    int          `json:"cycle_index"`
	Orientation   string       `json:"orientation"`
	Report        evidenceFile `json:"report"`
	Trace         evidenceFile `json:"trace"`
	Markers       evidenceFile `json:"decoded_markers"`
}

type matrixManifest struct {
	Schema                           string      `json:"schema"`
	GeneratedAt                      time.Time   `json:"generated_at"`
	SourceRevision                   string      `json:"source_revision"`
	NativePackageManifestSHA256      string      `json:"native_package_manifest_sha256"`
	NativePackageValidationMode      string      `json:"native_package_validation_mode"`
	NativeLocalTestCertificateSHA256 string      `json:"native_local_test_certificate_sha256,omitempty"`
	NativeDriverSHA256               string      `json:"native_driver_sha256"`
	NativeDriverBuildIdentity        string      `json:"native_driver_build_identity"`
	CycleID                          string      `json:"cycle_id"`
	CycleCount                       int         `json:"cycle_count"`
	CyclesPerPriority                int         `json:"cycles_per_priority"`
	SamplePairs                      int         `json:"sample_pairs_per_transition"`
	Runs                             []matrixRun `json:"runs"`
}

type evidenceOutput struct {
	Schema      string                     `json:"schema"`
	GeneratedAt time.Time                  `json:"generated_at"`
	Matrix      evidenceFile               `json:"matrix"`
	Analysis    *latency.SuperiorityReport `json:"analysis"`
	Verdict     string                     `json:"verdict"`
}

func main() {
	var input, output, source string
	flag.StringVar(&input, "input", "", "priority matrix JSON")
	flag.StringVar(&output, "output", "", "exclusive superiority evidence JSON")
	flag.StringVar(&source, "source", "", "expected repository revision")
	flag.Parse()
	if input == "" || output == "" || source == "" {
		fail(errors.New("input, output, and source are required"))
	}
	if !filepath.IsAbs(input) || !filepath.IsAbs(output) {
		fail(errors.New("matrix input and superiority output paths must be absolute"))
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			fail(fmt.Errorf("refusing to overwrite %s", output))
		}
		fail(err)
	}

	matrixIdentity, err := verifyEvidence(evidenceFileFromPath(input))
	if err != nil {
		fail(err)
	}
	file, err := os.Open(matrixIdentity.Path)
	if err != nil {
		fail(err)
	}
	matrix, err := parseMatrix(file)
	closeErr := file.Close()
	if err != nil {
		fail(err)
	}
	if closeErr != nil {
		fail(closeErr)
	}
	cycles, err := verifyMatrix(matrix, strings.ToLower(source))
	if err != nil {
		fail(err)
	}

	generatedAt := time.Now().UTC()
	analysis, err := latency.AnalyzeSuperiority(cycles, generatedAt)
	if err != nil {
		fail(err)
	}
	envelope := evidenceOutput{
		Schema: evidenceSchema, GeneratedAt: generatedAt,
		Matrix: matrixIdentity, Analysis: analysis, Verdict: analysis.Verdict,
	}
	if err = writeExclusive(output, &envelope); err != nil {
		fail(err)
	}
	if err = latency.RequireSuperiority(analysis); err != nil {
		fail(fmt.Errorf("superiority evidence retained at %s: %w", output, err))
	}
	fmt.Printf("strictly verified native latency was lower in all %d observed balanced cycles for this exact machine session\n",
		len(cycles))
}

func parseMatrix(reader io.Reader) (*matrixManifest, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var matrix matrixManifest
	if err := decoder.Decode(&matrix); err != nil {
		return nil, fmt.Errorf("decode priority matrix: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("priority matrix contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing priority matrix data: %w", err)
	}
	return &matrix, nil
}

func verifyMatrix(matrix *matrixManifest, source string) ([]latency.SuperiorityCycle, error) {
	if matrix.Schema != matrixSchema || matrix.GeneratedAt.IsZero() ||
		matrix.SourceRevision != source || !cycleIDPattern.MatchString(matrix.CycleID) ||
		!hashPattern.MatchString(matrix.NativePackageManifestSHA256) ||
		!hashPattern.MatchString(matrix.NativeDriverSHA256) ||
		!hashPattern.MatchString(matrix.NativeDriverBuildIdentity) ||
		(matrix.NativePackageValidationMode != latency.PackageValidationProduction &&
			matrix.NativePackageValidationMode != latency.PackageValidationLocalTest) ||
		(matrix.NativePackageValidationMode == latency.PackageValidationProduction &&
			matrix.NativeLocalTestCertificateSHA256 != "") ||
		(matrix.NativePackageValidationMode == latency.PackageValidationLocalTest &&
			!hashPattern.MatchString(matrix.NativeLocalTestCertificateSHA256)) ||
		matrix.CycleCount != len(matrix.Runs) || matrix.CycleCount%2 != 0 ||
		matrix.CyclesPerPriority < latency.SuperiorityMinimumCycles ||
		matrix.CyclesPerPriority%2 != 0 ||
		matrix.CycleCount != 2*matrix.CyclesPerPriority ||
		matrix.SamplePairs < latency.MinimumProductionSamplePairs ||
		matrix.SamplePairs > latency.MaximumProductionSamplePairs {
		return nil, errors.New("priority matrix header is incomplete or not production-strength")
	}
	seenPaths := make(map[string]bool, len(matrix.Runs)*3)
	cycles := make([]latency.SuperiorityCycle, 0, len(matrix.Runs))
	for runIndex, run := range matrix.Runs {
		cycleIndex := runIndex + 1
		wantPriority := "normal"
		wantPriorityCycle := cycleIndex
		if cycleIndex > matrix.CyclesPerPriority {
			wantPriority = "high"
			wantPriorityCycle -= matrix.CyclesPerPriority
		}
		wantOrientation := latency.ScheduleOrientationForCycle(cycleIndex)
		if run.PriorityClass != wantPriority || run.PriorityCycle != wantPriorityCycle ||
			run.CycleIndex != cycleIndex || run.Orientation != wantOrientation {
			return nil, fmt.Errorf("matrix run %d is not in canonical balanced order", cycleIndex)
		}
		verifiedFiles := make(map[string]evidenceFile, 3)
		for label, identity := range map[string]evidenceFile{
			"report": run.Report, "trace": run.Trace, "markers": run.Markers,
		} {
			verified, err := verifyEvidence(identity)
			if err != nil {
				return nil, fmt.Errorf("cycle %d %s: %w", cycleIndex, label, err)
			}
			canonical := strings.ToLower(verified.Path)
			if seenPaths[canonical] {
				return nil, fmt.Errorf("cycle %d reuses evidence path %s", cycleIndex, verified.Path)
			}
			seenPaths[canonical] = true
			verifiedFiles[label] = verified
		}
		reportFile, err := os.Open(run.Report.Path)
		if err != nil {
			return nil, err
		}
		suite, parseErr := latency.ParseSuiteReport(reportFile)
		closeErr := reportFile.Close()
		if parseErr != nil {
			return nil, fmt.Errorf("cycle %d report: %w", cycleIndex, parseErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err = latency.RequireSuitePass(suite); err != nil {
			return nil, fmt.Errorf("cycle %d report: %w", cycleIndex, err)
		}
		markerFile, err := os.Open(run.Markers.Path)
		if err != nil {
			return nil, err
		}
		markerEvidence, markerErr := latency.ParseTraceMarkerEvidence(markerFile)
		markerCloseErr := markerFile.Close()
		if markerErr != nil {
			return nil, fmt.Errorf("cycle %d markers: %w", cycleIndex, markerErr)
		}
		if markerCloseErr != nil {
			return nil, markerCloseErr
		}
		if markerEvidence.SourceTraceLength != verifiedFiles["trace"].Length ||
			markerEvidence.SourceTraceSHA256 != verifiedFiles["trace"].SHA256 {
			return nil, fmt.Errorf("cycle %d decoded markers are not bound to its raw ETL", cycleIndex)
		}
		if err = latency.VerifyTraceMarkers(suite, markerEvidence.Markers); err != nil {
			return nil, fmt.Errorf("cycle %d markers do not bind its report: %w", cycleIndex, err)
		}
		if len(suite.Cases) != 3 {
			return nil, fmt.Errorf("cycle %d controller set is incomplete", cycleIndex)
		}
		workload := suite.Cases[0].Workload
		if suite.Provenance.SourceRevision != source ||
			suite.Provenance.NativePackageManifestSHA256 != matrix.NativePackageManifestSHA256 ||
			suite.Provenance.NativePackageValidationMode != matrix.NativePackageValidationMode ||
			suite.Provenance.NativeLocalTestCertificateSHA256 != matrix.NativeLocalTestCertificateSHA256 ||
			suite.Provenance.NativeDriverSHA256 != matrix.NativeDriverSHA256 ||
			suite.Provenance.NativeDriverBuildIdentity != matrix.NativeDriverBuildIdentity ||
			suite.Provenance.Machine.ProcessPriorityClass != wantPriority ||
			workload.CycleID != matrix.CycleID || workload.CycleIndex != cycleIndex ||
			workload.CycleCount != matrix.CycleCount ||
			workload.ScheduleOrientation != wantOrientation ||
			workload.SamplePairs != matrix.SamplePairs {
			return nil, fmt.Errorf("cycle %d report contradicts its matrix receipt", cycleIndex)
		}
		cycles = append(cycles, latency.SuperiorityCycle{Priority: wantPriority, Suite: suite})
	}
	return cycles, nil
}

func evidenceFileFromPath(path string) evidenceFile {
	info, err := os.Lstat(path)
	if err != nil {
		return evidenceFile{Path: path}
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return evidenceFile{Path: path}
	}
	return evidenceFile{Path: path, Length: info.Size(), SHA256: digest}
}

func verifyEvidence(identity evidenceFile) (evidenceFile, error) {
	if !filepath.IsAbs(identity.Path) || identity.Length <= 0 ||
		!hashPattern.MatchString(identity.SHA256) {
		return evidenceFile{}, errors.New("evidence identity is incomplete or noncanonical")
	}
	info, err := os.Lstat(identity.Path)
	if err != nil {
		return evidenceFile{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != identity.Length {
		return evidenceFile{}, errors.New("evidence is not the exact nonempty regular file")
	}
	digest, err := fileSHA256(identity.Path)
	if err != nil {
		return evidenceFile{}, err
	}
	if digest != identity.SHA256 {
		return evidenceFile{}, fmt.Errorf("evidence SHA-256 %s does not match %s", digest, identity.SHA256)
	}
	canonical, err := filepath.Abs(identity.Path)
	if err != nil {
		return evidenceFile{}, err
	}
	identity.Path = filepath.Clean(canonical)
	return identity, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeExclusive(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, writeErr = file.Write(data); writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "latency matrix rejected:", err)
	os.Exit(1)
}
