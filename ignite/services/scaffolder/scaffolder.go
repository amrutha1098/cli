// Package scaffolder initializes Ignite CLI apps and modifies existing ones
// to add more features in a later time.
package scaffolder

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ignite/cli/ignite/chainconfig"
	sperrors "github.com/ignite/cli/ignite/errors"
	"github.com/ignite/cli/ignite/pkg/cache"
	"github.com/ignite/cli/ignite/pkg/cosmosanalysis"
	"github.com/ignite/cli/ignite/pkg/cosmosgen"
	"github.com/ignite/cli/ignite/pkg/cosmosver"
	"github.com/ignite/cli/ignite/pkg/gocmd"
	"github.com/ignite/cli/ignite/pkg/gomodule"
	"github.com/ignite/cli/ignite/pkg/gomodulepath"
)

// Scaffolder is Ignite CLI app scaffolder.
type Scaffolder struct {
	// Version of the chain
	Version cosmosver.Version

	// path of the app.
	path string

	// modpath represents the go module path of the app.
	modpath gomodulepath.Path
}

// App creates a new scaffolder for an existent app.
func App(path string) (Scaffolder, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Scaffolder{}, err
	}

	modpath, path, err := gomodulepath.Find(path)
	if err != nil {
		return Scaffolder{}, err
	}
	modfile, err := gomodule.ParseAt(path)
	if err != nil {
		return Scaffolder{}, err
	}
	if err := cosmosanalysis.ValidateGoMod(modfile); err != nil {
		return Scaffolder{}, err
	}

	version, err := cosmosver.Detect(path)
	if err != nil {
		return Scaffolder{}, err
	}

	if !version.IsFamily(cosmosver.Stargate) {
		return Scaffolder{}, sperrors.ErrOnlyStargateSupported
	}

	s := Scaffolder{
		Version: version,
		path:    path,
		modpath: modpath,
	}

	return s, nil
}

func finish(ctx context.Context, cacheStorage cache.Storage, path, gomodPath string) error {
	if err := protoc(ctx, cacheStorage, path, gomodPath); err != nil {
		return err
	}
	if err := gocmd.ModTidy(ctx, path); err != nil {
		return err
	}
	return gocmd.Fmt(ctx, path)
}

func protoc(ctx context.Context, cacheStorage cache.Storage, projectPath, gomodPath string) error {
	if err := cosmosgen.InstallDepTools(ctx, projectPath); err != nil {
		return err
	}

	confpath, err := chainconfig.LocateDefault(projectPath)
	if err != nil {
		return err
	}
	conf, err := chainconfig.ParseFile(confpath)
	if err != nil {
		return err
	}

	options := []cosmosgen.Option{
		cosmosgen.WithGoGeneration(gomodPath),
		cosmosgen.IncludeDirs(conf.Build.Proto.ThirdPartyPaths),
	}

	// Generate Typescript client code if it's enabled or when Vuex stores are generated
	if conf.Client.Typescript.Path != "" || conf.Client.Vuex.Path != "" {
		tsClientPath := chainconfig.TSClientPath(conf)
		if !filepath.IsAbs(tsClientPath) {
			tsClientPath = filepath.Join(projectPath, tsClientPath)
		}

		if err := os.MkdirAll(tsClientPath, 0o766); err != nil {
			return err
		}

		options = append(options,
			cosmosgen.WithTSClientGeneration(
				cosmosgen.TypescriptModulePath(tsClientPath),
				tsClientPath,
			),
		)
	}

	if conf.Client.Vuex.Path != "" {
		vuexPath := conf.Client.Vuex.Path
		if filepath.IsAbs(vuexPath) {
			vuexPath = filepath.Join(vuexPath, "generated")
		} else {
			vuexPath = filepath.Join(projectPath, vuexPath, "generated")
		}

		options = append(options,
			cosmosgen.WithVuexGeneration(
				false,
				cosmosgen.TypescriptModulePath(vuexPath),
				vuexPath,
			),
		)
	}

	if conf.Client.OpenAPI.Path != "" {
		openAPIPath := conf.Client.OpenAPI.Path
		if !filepath.IsAbs(openAPIPath) {
			openAPIPath = filepath.Join(projectPath, openAPIPath)
		}

		options = append(options, cosmosgen.WithOpenAPIGeneration(openAPIPath))
	}

	return cosmosgen.Generate(ctx, cacheStorage, projectPath, conf.Build.Proto.Path, options...)
}
