package godocker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/wyattis/z/zrand"
)

type Opt = func(config Config) Config

type Config struct {
	Name            string
	Cwd             string
	Host            *container.HostConfig
	Network         *network.NetworkingConfig
	Platform        *v1.Platform
	ContainerConfig *container.Config
	ImageBuildOpts  *types.ImageBuildOptions
	ImagePullOpts   *types.ImagePullOptions
	Ctx             context.Context
	ClientOpts      []client.Opt

	Cli       *client.Client
	Container *container.CreateResponse
	ImageTags []string

	// internal
	usingAPIVersion bool
	execRunners     []func(config Config) (err error)
	afterRunners    []func(config Config) (err error)
}

func WithClientOpts(opts ...client.Opt) Opt {
	return func(config Config) Config {
		config.ClientOpts = append(config.ClientOpts, opts...)
		return config
	}
}

func WithCtx(ctx context.Context) Opt {
	return func(config Config) Config {
		config.Ctx = ctx
		return config
	}
}

func WithAPIVersion(version string) Opt {
	return func(config Config) Config {
		config.usingAPIVersion = true
		config.ClientOpts = append(config.ClientOpts, client.WithVersion(version))
		return config
	}
}

func WithCwd(cwd string) Opt {
	return func(config Config) Config {
		config.Cwd = cwd
		return config
	}
}

func Exec(runners ...func(config Config) (err error)) Opt {
	return func(config Config) Config {
		config.execRunners = append(config.execRunners, runners...)
		return config
	}
}

func After(runners ...func(config Config) (err error)) Opt {
	return func(config Config) Config {
		config.afterRunners = append(config.afterRunners, runners...)
		return config
	}
}

func WithDockerfile(path string) Opt {
	return func(config Config) Config {
		if config.ImageBuildOpts == nil {
			config.ImageBuildOpts = &types.ImageBuildOptions{}
		}
		config.ImageBuildOpts.Dockerfile = path
		return config
	}
}

func WithImage(image string) Opt {
	return func(config Config) Config {
		if config.Container == nil {
			config.ContainerConfig = &container.Config{}
		}
		config.ContainerConfig.Image = image
		return config
	}
}

func WithConfig(config Config) Opt {
	return func(_ Config) Config {
		return config
	}
}

func CleanupContainer() Opt {
	return func(config Config) Config {
		config.afterRunners = append(config.afterRunners, func(config Config) (err error) {
			fmt.Println("removing container")
			if err = config.Cli.ContainerRemove(config.Ctx, config.Container.ID, types.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
				return
			}
			return
		})
		return config
	}
}

func CleanupImage() Opt {
	return func(config Config) Config {
		config.afterRunners = append(config.afterRunners, func(config Config) (err error) {
			opts := types.ImageListOptions{
				Filters: filters.NewArgs(),
			}
			if config.ContainerConfig != nil && config.ContainerConfig.Image != "" {
				opts.Filters.Add("reference", config.ContainerConfig.Image)
			}
			if len(config.ImageTags) > 0 {
				for _, tag := range config.ImageTags {
					opts.Filters.Add("label", tag)
				}
			}
			list, err := config.Cli.ImageList(config.Ctx, opts)
			if err != nil {
				return err
			}
			fmt.Println("removing images", len(list))
			for _, image := range list {
				fmt.Println("removing image", image.ID)
				if _, err = config.Cli.ImageRemove(config.Ctx, image.ID, types.ImageRemoveOptions{PruneChildren: true}); err != nil {
					return err
				}
			}
			return
		})
		return config
	}
}

func loadContext(config Config) (context io.Reader, err error) {
	context = NewTarDirReader(config.Cwd)
	return
}

type streamLine struct {
	Stream         string `json:"stream"`
	Status         string `json:"status"`
	ProgressDetail struct {
		Current int `json:"current"`
		Total   int `json:"total"`
	} `json:"progressDetail"`
	Progress string `json:"progress"`
	Id       string `json:"id"`
	Aux      struct {
		ID string `json:"ID"`
	} `json:"aux"`
}

func (s streamLine) IsStream() bool {
	return s.Stream != ""
}

func consumeDockerStream(reader io.ReadCloser, lines chan<- streamLine) (err error) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if err = scanner.Err(); err != nil {
			return
		}
		line := scanner.Bytes()
		var s streamLine
		if err = json.Unmarshal(line, &s); err != nil {
			return
		}
		if s.IsStream() {
			fmt.Println(s.Stream)
		} else {
			lines <- s
		}
	}
	close(lines)
	return
}

func buildImage(config Config) (imageTag string, err error) {
	if config.Cwd == "" {
		config.Cwd, err = os.Getwd()
		if err != nil {
			return
		}
	}
	config.Cwd, err = filepath.Abs(config.Cwd)
	if err != nil {
		return
	}
	fmt.Println("building image from", config.Cwd)
	config.ImageBuildOpts.Tags = append(config.ImageBuildOpts.Tags, config.ImageBuildOpts.BuildID)
	config.ImageBuildOpts.BuildID = strings.ToLower(zrand.AlphaWord(6))
	imageTag = config.ImageBuildOpts.BuildID
	if config.ImageBuildOpts.Context == nil {
		config.ImageBuildOpts.Context, err = loadContext(config)
		if err != nil {
			return "", err
		}
	}

	buildComplete := false
	buildContext, err := loadContext(config)
	if err != nil {
		return
	}
	res, err := config.Cli.ImageBuild(config.Ctx, buildContext, *config.ImageBuildOpts)
	if err != nil {
		return
	}
	lines := make(chan streamLine)
	defer func() {
		if !buildComplete {
			fmt.Println("build cancelled")
			if err := config.Cli.BuildCancel(config.Ctx, config.ImageBuildOpts.BuildID); err != nil {
				panic(err)
			}
		}
	}()

	// This closes both resources once they are fully consumed
	go consumeDockerStream(res.Body, lines)

	// Pull the image id out of the stream
	for l := range lines {
		if l.Aux.ID != "" {
			imageTag = l.Aux.ID
		}
	}

	buildComplete = true
	return
}

func pullImage(config Config) (err error) {
	if config.ImagePullOpts == nil {
		config.ImagePullOpts = &types.ImagePullOptions{}
	}
	if config.ContainerConfig == nil || config.ContainerConfig.Image == "" {
		return errors.New("no image specified")
	}
	fmt.Println("pulling image", config.ContainerConfig.Image)
	out, err := config.Cli.ImagePull(config.Ctx, config.ContainerConfig.Image, *config.ImagePullOpts)
	if err != nil {
		return
	}
	defer out.Close()
	_, err = io.Copy(os.Stdout, out)
	return
}

func Run(opts ...Opt) (err error) {
	var config Config
	for _, opt := range opts {
		config = opt(config)
	}

	if config.Ctx == nil {
		config.Ctx = context.Background()
	}

	if !config.usingAPIVersion {
		config.ClientOpts = append(config.ClientOpts, client.WithAPIVersionNegotiation())
	}

	config.Cli, err = client.NewClientWithOpts(config.ClientOpts...)
	if err != nil {
		return
	}
	defer config.Cli.Close()

	defer func() {
		for _, after := range config.afterRunners {
			if err = after(config); err != nil {
				return
			}
		}
	}()

	if config.ImageBuildOpts != nil {
		imageTag, err := buildImage(config)
		if err != nil {
			return err
		}
		if config.ContainerConfig == nil {
			config.ContainerConfig = &container.Config{}
		}
		config.ContainerConfig.Image = imageTag
		config.ImageTags = append(config.ImageTags, imageTag)
	} else if config.ContainerConfig != nil && config.ContainerConfig.Image != "" {
		if err = pullImage(config); err != nil {
			return
		}
	} else {
		return fmt.Errorf("no image build or pull options provided")
	}

	if err = execContainer(&config); err != nil {
		return
	}

	return
}

func execContainer(config *Config) (err error) {
	fmt.Println("creating container")
	c, err := config.Cli.ContainerCreate(config.Ctx, config.ContainerConfig, config.Host, config.Network, config.Platform, config.Name)
	if err != nil {
		return
	}

	config.Container = &c

	fmt.Println("starting container")
	if err = config.Cli.ContainerStart(config.Ctx, config.Container.ID, types.ContainerStartOptions{}); err != nil {
		return
	}

	defer func() {
		fmt.Println("stopping container")
		if err := config.Cli.ContainerStop(config.Ctx, config.Container.ID, container.StopOptions{}); err != nil {
			panic(err)
		}
	}()

	for _, runner := range config.execRunners {
		if err = runner(*config); err != nil {
			return
		}
	}
	return
}
