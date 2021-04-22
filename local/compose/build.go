/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compose

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/compose-spec/compose-go/types"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/buildx/build"
	"github.com/docker/buildx/driver"
	_ "github.com/docker/buildx/driver/docker" // required to get default driver registered
	"github.com/docker/buildx/util/progress"
	cliconfig "github.com/docker/cli/cli/config"
	moby "github.com/docker/docker/api/types"
	bclient "github.com/moby/buildkit/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/docker/compose-cli/api/compose"
	"github.com/docker/compose-cli/api/config"
	composeprogress "github.com/docker/compose-cli/api/progress"
	"github.com/docker/compose-cli/cli/metrics"
	"github.com/docker/compose-cli/utils"
)

func (s *composeService) Build(ctx context.Context, project *types.Project, options compose.BuildOptions) error {
	opts := map[string]build.Options{}
	imagesToBuild := []string{}

	for _, service := range project.Services {
		if service.Build != nil {
			imageName := getImageName(service, project.Name)
			imagesToBuild = append(imagesToBuild, imageName)
			buildOptions, err := s.toBuildOptions(service, imageName)
			if err != nil {
				return err
			}
			buildOptions.Pull = options.Pull
			buildOptions.BuildArgs = options.Args
			buildOptions.NoCache = options.NoCache
			opts[imageName] = buildOptions
			buildOptions.CacheFrom, err = build.ParseCacheEntry(service.Build.CacheFrom)
			if err != nil {
				return err
			}

			for _, image := range service.Build.CacheFrom {
				buildOptions.CacheFrom = append(buildOptions.CacheFrom, bclient.CacheOptionsEntry{
					Type:  "registry",
					Attrs: map[string]string{"ref": image},
				})
			}
		}
	}

	_, err := s.build(ctx, project, opts, Containers{}, options.Progress)
	if err == nil {
		if len(imagesToBuild) > 0 && !options.Quiet {
			utils.DisplayScanSuggestMsg()
		}
	}

	return err
}

func (s *composeService) ensureImagesExists(ctx context.Context, project *types.Project, observedState Containers, quietPull bool) error {
	images, err := s.getImageDigests(ctx, project)
	if err != nil {
		return err
	}

	mode := progress.PrinterModeAuto
	if quietPull {
		mode = progress.PrinterModeQuiet
	}
	opts, imagesToBuild, err := s.getBuildOptions(project, images)
	if err != nil {
		return err
	}
	builtImages, err := s.build(ctx, project, opts, observedState, mode)
	if err != nil {
		return err
	}

	if len(imagesToBuild) > 0 {
		utils.DisplayScanSuggestMsg()
	}
	for name, digest := range builtImages {
		images[name] = digest
	}
	// set digest as service.Image
	for i, service := range project.Services {
		digest, ok := images[getImageName(service, project.Name)]
		if ok {
			project.Services[i].Image = digest
		}
	}
	return nil
}

func (s *composeService) getBuildOptions(project *types.Project, images map[string]string) (map[string]build.Options, []string, error) {
	opts := map[string]build.Options{}
	imagesToBuild := []string{}
	for _, service := range project.Services {
		if service.Image == "" && service.Build == nil {
			return nil, nil, fmt.Errorf("invalid service %q. Must specify either image or build", service.Name)
		}
		imageName := getImageName(service, project.Name)
		_, localImagePresent := images[imageName]

		if service.Build != nil {
			if localImagePresent && service.PullPolicy != types.PullPolicyBuild {
				continue
			}
			imagesToBuild = append(imagesToBuild, imageName)
			opt, err := s.toBuildOptions(service, imageName)
			if err != nil {
				return nil, nil, err
			}
			opts[imageName] = opt
			continue
		}
		if service.Image != "" {
			if localImagePresent {
				continue
			}
		}
		// Buildx has no command to "just pull", see
		// so we bake a temporary dockerfile that will just pull and export pulled image
		opts[service.Name] = build.Options{
			Inputs: build.Inputs{
				ContextPath:    ".",
				DockerfilePath: "-",
				InStream:       strings.NewReader("FROM " + service.Image),
			},
			Tags: []string{service.Image}, // Used to retrieve image to pull in case of windows engine
			Pull: true,
		}

	}
	return opts, imagesToBuild, nil

}

func (s *composeService) getImageDigests(ctx context.Context, project *types.Project) (map[string]string, error) {
	imageNames := []string{}
	for _, s := range project.Services {
		imgName := getImageName(s, project.Name)
		if !utils.StringContains(imageNames, imgName) {
			imageNames = append(imageNames, imgName)
		}
	}
	imgs, err := s.getImages(ctx, imageNames)
	if err != nil {
		return nil, err
	}
	images := map[string]string{}
	for name, info := range imgs {
		images[name] = info.ID
	}
	return images, nil
}

func (s *composeService) build(ctx context.Context, project *types.Project, opts map[string]build.Options, observedState Containers, mode string) (map[string]string, error) {
	info, err := s.apiClient.Info(ctx)
	if err != nil {
		return nil, err
	}

	if info.OSType == "windows" {
		// no support yet for Windows container builds in Buildkit
		// https://docs.docker.com/develop/develop-images/build_enhancements/#limitations
		err := s.windowsBuild(opts, mode)
		return nil, metrics.WrapCategorisedComposeError(err, metrics.BuildFailure)
	}
	if len(opts) == 0 {
		return nil, nil
	}
	const drivername = "default"

	configFile, err := cliconfig.Load(config.Dir())
	if err != nil {
		return nil, err
	}

	d, err := driver.GetDriver(ctx, drivername, nil, s.apiClient, configFile, nil, nil, "", nil, nil, project.WorkingDir)
	if err != nil {
		return nil, err
	}
	driverInfo := []build.DriverInfo{
		{
			Name:   "default",
			Driver: d,
		},
	}

	// Progress needs its own context that lives longer than the
	// build one otherwise it won't read all the messages from
	// build and will lock
	progressCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := progress.NewPrinter(progressCtx, os.Stdout, mode)

	// We rely on buildx "docker" builder integrated in docker engine, so don't need a DockerAPI here
	response, err := build.Build(ctx, driverInfo, opts, nil, nil, w)
	errW := w.Wait()
	if err == nil {
		err = errW
	}
	if err != nil {
		return nil, metrics.WrapCategorisedComposeError(err, metrics.BuildFailure)
	}

	cw := composeprogress.ContextWriter(ctx)
	for _, c := range observedState {
		for imageName := range opts {
			if c.Image == imageName {
				err = s.removeContainers(ctx, cw, []moby.Container{c}, nil)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	imagesBuilt := map[string]string{}
	for name, img := range response {
		if img == nil || len(img.ExporterResponse) == 0 {
			continue
		}
		digest, ok := img.ExporterResponse["containerimage.digest"]
		if !ok {
			continue
		}
		imagesBuilt[name] = digest
	}

	return imagesBuilt, err
}

func (s *composeService) toBuildOptions(service types.ServiceConfig, imageTag string) (build.Options, error) {
	var tags []string
	tags = append(tags, imageTag)

	var buildArgs map[string]string

	var plats []specs.Platform
	if service.Platform != "" {
		p, err := platforms.Parse(service.Platform)
		if err != nil {
			return build.Options{}, err
		}
		plats = append(plats, p)
	}

	return build.Options{
		Inputs: build.Inputs{
			ContextPath:    service.Build.Context,
			DockerfilePath: service.Build.Dockerfile,
		},
		BuildArgs: flatten(mergeArgs(service.Build.Args, buildArgs)),
		Tags:      tags,
		Target:    service.Build.Target,
		Exports:   []bclient.ExportEntry{{Type: "image", Attrs: map[string]string{}}},
		Platforms: plats,
		Labels:    service.Build.Labels,
	}, nil
}

func flatten(in types.MappingWithEquals) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = *v
	}
	return out
}

func mergeArgs(src types.MappingWithEquals, values map[string]string) types.MappingWithEquals {
	for key := range src {
		if val, ok := values[key]; ok {
			if val == "" {
				src[key] = nil
			} else {
				src[key] = &val
			}
		}
	}
	return src
}