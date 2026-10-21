package main

import (
	"errors"
	"os"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/harness"
	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/runtime"
)

// Reload config.json for each analysis so model switches apply on the next save. Flags must pass o.config first.
func (o *options) configure(dataDir, selfExe string) func() harness.Config {
	return func() harness.Config { return o.resolve(dataDir, selfExe) }
}

// Missing model prerequisites disable advice with a reason while preserving exact analysis.
func (o *options) resolve(dataDir, selfExe string) harness.Config {
	conf, confErr := app.DefaultConfig(), error(nil)
	if dataDir != "" {
		if c, err := app.LoadConfig(dataDir); err == nil {
			conf = c
		} else {
			confErr = err
		}
	}
	tests, timeout := conf.Tests, conf.AdvisoryTimeout
	if o.set["tests"] {
		tests = o.tests
	}
	if o.set["advisory-timeout"] {
		timeout = o.timeout
	}
	cfg, _ := o.config()
	cfg.Build.TestTargets = tests
	if cmd, ok := cfg.Provider.(advisory.Command); ok {
		cmd.Timeout = timeout
		cfg.Provider = cmd
	}
	if o.advisory != "model" {
		return cfg
	}

	disabled := func(reason string) harness.Config {
		cfg.Disabled, cfg.Key = reason, "model: "+reason
		return cfg
	}
	if confErr != nil {
		return disabled(confErr.Error())
	}
	id := o.model
	if id == "" {
		id = conf.Model
	}
	if id == "" {
		return disabled(models.ErrNoModel.Error())
	}
	m, dir, err := models.Bundle(dataDir, id)
	if err != nil {
		return disabled(err.Error())
	}
	argv, err := runtime.Argv(dataDir, dir, selfExe, conf.Threads)
	if errors.Is(err, runtime.ErrNotSetUp) {
		return disabled(runtime.ErrNotSetUp.Error())
	}
	if err != nil {
		return disabled(err.Error())
	}
	cfg.Provider, cfg.Key = advisory.Command{Argv: argv, Timeout: timeout}, m.ID
	return cfg
}

// Only the model provider requires an installation; other providers can use configuration defaults.
func (o *options) installation() (dataDir, selfExe string, err error) {
	dataDir, err = app.DataDir()
	if err != nil {
		if o.advisory == "model" {
			return "", "", err
		}
		dataDir = ""
	}
	selfExe, err = os.Executable()
	if err != nil {
		selfExe = os.Args[0]
	}
	return dataDir, selfExe, nil
}
