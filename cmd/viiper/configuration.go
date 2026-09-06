package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/alecthomas/kong"
	kongtoml "github.com/alecthomas/kong-toml"
	kongyaml "github.com/alecthomas/kong-yaml"
	"gopkg.in/yaml.v3"
)

// Configuration selection precedes Kong configuration loading, logger creation,
// update checks, and server startup. Only a command-line opt-in may narrow it.
func configurationOptions(args []string) ([]kong.Option, bool, error) {
	path, only, err := exclusiveConfigArgument(args)
	if err != nil {
		return nil, only, err
	}
	if only {
		resolver, err := loadExclusiveConfig(path)
		if err != nil {
			return nil, true, err
		}
		return []kong.Option{kong.Resolvers(resolver)}, true, nil
	}
	jsonPaths, yamlPaths, tomlPaths := configpaths.ConfigCandidatePaths(findUserConfig(args))
	return []kong.Option{
		kong.Configuration(kong.JSON, jsonPaths...),
		kong.Configuration(kongyaml.Loader, yamlPaths...),
		kong.Configuration(kongtoml.Loader, tomlPaths...),
	}, false, nil
}

func exclusiveConfigArgument(args []string) (string, bool, error) {
	only, seen := false, false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg != "--config-only" && !strings.HasPrefix(arg, "--config-only=") {
			continue
		}
		if seen {
			return "", true, fmt.Errorf("--config-only must be supplied once")
		}
		seen, only = true, true
		if value, present := strings.CutPrefix(arg, "--config-only="); present {
			var err error
			only, err = strconv.ParseBool(value)
			if err != nil {
				return "", true, fmt.Errorf("invalid --config-only value: %w", err)
			}
		}
	}
	if !only {
		return "", false, nil
	}
	path, count := "", 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if value, present := strings.CutPrefix(arg, "--config="); present {
			path, count = value, count+1
		} else if arg == "--config" {
			count++
			if i+1 < len(args) {
				i++
				path = args[i]
			}
		}
	}
	if count != 1 {
		return "", true, fmt.Errorf("--config-only requires exactly one explicit absolute --config file")
	}
	return path, true, nil
}

func loadExclusiveConfig(path string) (kong.Resolver, error) {
	path, err := configpaths.ExplicitConfigFilePath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read explicit configuration: %w", err)
	}
	var loader kong.ConfigurationLoader
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		// Kong's JSON loader reads one value. Require one complete document
		// so trailing garbage or a second document cannot be silently ignored.
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			return nil, fmt.Errorf("invalid explicit JSON configuration: %w", err)
		}
		if object == nil {
			return nil, fmt.Errorf("explicit JSON configuration must be an object")
		}
		loader = kong.JSON
	case ".yaml", ".yml":
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			return nil, fmt.Errorf("invalid explicit YAML configuration: %w", err)
		}
		if object == nil {
			return nil, fmt.Errorf("explicit YAML configuration must be a mapping")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("explicit YAML configuration must contain exactly one document")
		}
		loader = kongyaml.Loader
	case ".toml":
		loader = kongtoml.Loader
	default:
		return nil, fmt.Errorf("explicit configuration requires a .json, .yaml, .yml, or .toml extension")
	}
	resolver, err := loader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid explicit configuration %s: %w", path, err)
	}
	return resolver, nil
}
