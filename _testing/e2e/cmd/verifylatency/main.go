package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
)

func main() {
	var input, markersPath, source, sdlRevision, sdlHash, manifestHash, driverHash, driverBuildIdentity, profileHash string
	var samples int
	flag.StringVar(&input, "input", "", "latency suite JSON")
	flag.StringVar(&markersPath, "markers", "", "decoded ETL TraceLogging marker JSON")
	flag.StringVar(&source, "source", "", "expected repository revision")
	flag.StringVar(&sdlRevision, "sdl-revision", "", "expected SDL revision")
	flag.StringVar(&sdlHash, "sdl-sha256", "", "expected loaded SDL SHA-256")
	flag.StringVar(&manifestHash, "manifest-sha256", "", "expected package manifest SHA-256")
	flag.StringVar(&driverHash, "driver-sha256", "", "expected installed driver SHA-256")
	flag.StringVar(&driverBuildIdentity, "driver-build-identity", "", "expected negotiated loaded-driver identity")
	flag.StringVar(&profileHash, "trace-profile-sha256", "", "expected WPRP SHA-256")
	flag.IntVar(&samples, "samples", 0, "expected sample pairs per controller/transport")
	flag.Parse()
	if input == "" || markersPath == "" || source == "" || sdlRevision == "" || sdlHash == "" ||
		manifestHash == "" || driverHash == "" || driverBuildIdentity == "" || profileHash == "" || samples == 0 {
		fail(errors.New("all verifier flags are required"))
	}
	file, err := os.Open(input)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	suite, err := latency.ParseSuiteReport(file)
	if err != nil {
		fail(err)
	}
	if err = latency.RequireSuitePass(suite); err != nil {
		fail(err)
	}
	p := suite.Provenance
	if p.SourceRevision != strings.ToLower(source) ||
		p.SDLSourceRevision != strings.ToLower(sdlRevision) ||
		p.SDLBinarySHA256 != strings.ToLower(sdlHash) ||
		p.NativePackageManifestSHA256 != strings.ToLower(manifestHash) ||
		p.NativeDriverSHA256 != strings.ToLower(driverHash) ||
		p.NativeDriverBuildIdentity != strings.ToLower(driverBuildIdentity) ||
		p.TraceProfileSHA256 != strings.ToLower(profileHash) ||
		p.TraceProviderName != latency.TraceProviderName ||
		p.TraceProviderGUID != latency.TraceProviderGUID ||
		p.USBIPBaselineMode != latency.USBIPBaselineMode ||
		p.USBIPBaselineVersion != latency.USBIPBaselineVersion {
		fail(errors.New("suite provenance does not match the production invocation"))
	}
	for _, controllerCase := range suite.Cases {
		if controllerCase.Workload.SamplePairs != samples {
			fail(fmt.Errorf("%s has %d sample pairs, want %d",
				controllerCase.Workload.ControllerType, controllerCase.Workload.SamplePairs, samples))
		}
	}
	markersFile, err := os.Open(markersPath)
	if err != nil {
		fail(err)
	}
	defer markersFile.Close()
	markers, err := latency.ParseTraceMarkers(markersFile)
	if err != nil {
		fail(err)
	}
	if err = latency.VerifyTraceMarkers(suite, markers); err != nil {
		fail(err)
	}
	fmt.Printf("strictly verified %d controller cases\n", len(suite.Cases))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "latency evidence rejected:", err)
	os.Exit(1)
}
