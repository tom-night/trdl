package trdl

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/werf/logboek"

	"github.com/werf/vault-plugin-secrets-trdl/pkg/config"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/docker"
	trdlGit "github.com/werf/vault-plugin-secrets-trdl/pkg/git"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/pgp"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/tasks_manager"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/util"
)

const (
	fieldNameGitTag      = "git_tag"
	fieldNameGitUsername = "git_username"
	fieldNameGitPassword = "git_password"
)

func releasePath(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: `release$`,
		Fields: map[string]*framework.FieldSchema{
			fieldNameGitTag: {
				Type:        framework.TypeString,
				Description: "Project git repository tag which should be released",
				Required:    true,
			},
			fieldNameGitUsername: {
				Type:        framework.TypeString,
				Description: "Git username",
			},
			fieldNameGitPassword: {
				Type:        framework.TypeString,
				Description: "Git password",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathRelease,
				Summary:  pathReleaseHelpSyn,
			},
		},

		HelpSynopsis:    pathReleaseHelpSyn,
		HelpDescription: pathReleaseHelpDesc,
	}
}

func (b *backend) pathRelease(ctx context.Context, req *logical.Request, fields *framework.FieldData) (*logical.Response, error) {
	if err := util.CheckRequiredFields(req, fields); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	cfg, err := getConfiguration(ctx, req.Storage)
	if err != nil {
		return nil, fmt.Errorf("unable to get configuration from storage: %s", err)
	}

	if cfg == nil {
		return logical.ErrorResponse("configuration not found"), nil
	}

	gitCredentialFromStorage, err := getGitCredential(ctx, req.Storage)
	if err != nil {
		return nil, fmt.Errorf("unable to get git credential from storage: %s", err)
	}

	gitTag := fields.Get(fieldNameGitTag).(string)
	gitUsername := fields.Get(fieldNameGitUsername).(string)
	gitPassword := fields.Get(fieldNameGitPassword).(string)
	if gitCredentialFromStorage != nil && gitUsername == "" && gitPassword == "" {
		gitUsername = gitCredentialFromStorage.Username
		gitPassword = gitCredentialFromStorage.Password
	}

	publisherRepository, err := b.Publisher.GetRepository(ctx, req.Storage, cfg.RepositoryOptions())
	if err != nil {
		return nil, fmt.Errorf("error getting publisher repository: %s", err)
	}

	taskUUID, err := b.TasksManager.RunTask(context.Background(), req.Storage, func(ctx context.Context, storage logical.Storage) error {
		logboek.Context(ctx).Default().LogF("Started task\n")
		hclog.L().Debug("Started task")

		gitRepo, err := cloneGitRepositoryTag(cfg.GitRepoUrl, gitTag, gitUsername, gitPassword)
		if err != nil {
			return fmt.Errorf("unable to clone git repository: %s", err)
		}

		logboek.Context(ctx).Default().LogF("Cloned git repo\n")
		hclog.L().Debug("Cloned git repo")

		trustedPGPPublicKeys, err := pgp.GetTrustedPGPPublicKeys(ctx, req.Storage)
		if err != nil {
			return fmt.Errorf("unable to get trusted pgp public keys: %s", err)
		}

		if err := trdlGit.VerifyTagSignatures(gitRepo, gitTag, trustedPGPPublicKeys, cfg.RequiredNumberOfVerifiedSignaturesOnCommit); err != nil {
			return fmt.Errorf("signature verification failed: %s", err)
		}

		logboek.Context(ctx).Default().LogF("Verified tag signatures\n")
		hclog.L().Debug("Verified tag signatures")

		trdlCfg, err := getTrdlConfig(gitRepo, gitTag)
		if err != nil {
			return fmt.Errorf("unable to get trdl configuration: %s", err)
		}

		logboek.Context(ctx).Default().LogF("Got trdl.yaml configuration\n")

		tarReader, tarWriter := io.Pipe()
		if err := buildReleaseArtifacts(ctx, tarWriter, gitRepo, trdlCfg.DockerImage, trdlCfg.Commands); err != nil {
			return fmt.Errorf("unable to build release artifacts: %s", err)
		}

		logboek.Context(ctx).Default().LogF("Built release artifacts tar archive\n")
		hclog.L().Debug("Built release artifacts tar archive")

		{
			twArtifacts := tar.NewReader(tarReader)
			for {
				hdr, err := twArtifacts.Next()

				logboek.Context(ctx).Default().LogF("Next tar entry hdr=%#v err=%v\n", hdr, err)
				hclog.L().Debug(fmt.Sprintf("Next tar entry hdr=%#v err=%v", hdr, err))

				if err == io.EOF {
					break
				}

				if err != nil {
					return fmt.Errorf("error reading next tar artifact header: %s", err)
				}

				if hdr.Typeflag != tar.TypeDir {
					logboek.Context(ctx).Default().LogF("Publishing %q into the tuf repo ...\n", hdr.Name)
					hclog.L().Debug(fmt.Sprintf("Publishing %q into the tuf repo ...", hdr.Name))

					if err := b.Publisher.PublishReleaseTarget(ctx, publisherRepository, gitTag, hdr.Name, twArtifacts); err != nil {
						return fmt.Errorf("unable to publish release target %q: %s", hdr.Name, err)
					}

					logboek.Context(ctx).Default().LogF("Published %q into the tuf repo\n", hdr.Name)
					hclog.L().Debug(fmt.Sprintf("Published %q into the tuf repo", hdr.Name))
				}
			}

			if err := publisherRepository.Commit(ctx); err != nil {
				return fmt.Errorf("unable to commit new tuf repository state: %s", err)
			}

			logboek.Context(ctx).Default().LogF("Tuf repo commit done\n")
			hclog.L().Debug("Tuf repo commit done")
		}

		return nil
	})
	if err != nil {
		if err == tasks_manager.ErrBusy {
			return logical.ErrorResponse(err.Error()), nil
		}

		return nil, err
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"task_uuid": taskUUID,
		},
	}, nil
}

func cloneGitRepositoryTag(url, gitTag, username, password string) (*git.Repository, error) {
	cloneGitOptions := trdlGit.CloneOptions{
		TagName:           gitTag,
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
	}

	if username != "" && password != "" {
		cloneGitOptions.Auth = &http.BasicAuth{
			Username: username,
			Password: password,
		}
	}

	gitRepo, err := trdlGit.CloneInMemory(url, cloneGitOptions)
	if err != nil {
		return nil, err
	}

	return gitRepo, nil
}

func getTrdlConfig(gitRepo *git.Repository, gitTag string) (*config.Trdl, error) {
	data, err := trdlGit.ReadWorktreeFile(gitRepo, config.TrdlFileName)
	if err != nil {
		return nil, fmt.Errorf("unable to read worktree file %q: %s", config.TrdlFileName, err)
	}

	values := map[string]interface{}{
		"Tag": gitTag,
	}

	cfg, err := config.ParseTrdl(data, values)
	if err != nil {
		return nil, fmt.Errorf("error parsing %q configuration file: %s", config.TrdlFileName, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("error validation %q configuration file: %s", config.TrdlFileName, err)
	}

	return cfg, nil
}

func buildReleaseArtifacts(ctx context.Context, tarWriter *io.PipeWriter, gitRepo *git.Repository, fromImage string, runCommands []string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("unable to create docker client: %s", err)
	}

	serviceDirInContext := ".trdl"
	serviceDockerfilePathInContext := path.Join(serviceDirInContext, "Dockerfile")
	contextReader, contextWriter := io.Pipe()
	go func() {
		if err := func() error {
			tw := tar.NewWriter(contextWriter)

			if err := trdlGit.AddWorktreeFilesToTar(tw, gitRepo); err != nil {
				return fmt.Errorf("unable to add git worktree files to tar: %s", err)
			}

			dockerfileOpts := docker.DockerfileOpts{
				WithArtifacts: true,
			}
			if err := docker.GenerateAndAddDockerfileToTar(tw, serviceDockerfilePathInContext, fromImage, runCommands, dockerfileOpts); err != nil {
				return fmt.Errorf("unable to add service dockerfile to tar: %s", err)
			}

			if err := tw.Close(); err != nil {
				return fmt.Errorf("unable to close tar writer: %s", err)
			}

			return nil
		}(); err != nil {
			if closeErr := contextWriter.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := contextWriter.Close(); err != nil {
			panic(err)
		}
	}()

	response, err := cli.ImageBuild(ctx, contextReader, types.ImageBuildOptions{
		Dockerfile:  serviceDockerfilePathInContext,
		PullParent:  true,
		NoCache:     true,
		Remove:      true,
		ForceRemove: true,
		Version:     types.BuilderV1,
	})
	if err != nil {
		return fmt.Errorf("unable to run docker image build: %s", err)
	}

	handleFromImageBuildResponse(response, tarWriter)

	return nil
}

func handleFromImageBuildResponse(response types.ImageBuildResponse, tarWriter *io.PipeWriter) {
	r, w := io.Pipe()
	go func() {
		if err := docker.ReadTarFromImageBuildResponse(w, response); err != nil {
			if closeErr := w.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := w.Close(); err != nil {
			panic(err)
		}
	}()

	go func() {
		decoder := base64.NewDecoder(base64.StdEncoding, r)
		if _, err := io.Copy(tarWriter, decoder); err != nil {
			if closeErr := tarWriter.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := w.Close(); err != nil {
			panic(err)
		}
	}()
}

const (
	pathReleaseHelpSyn = `
	Performs release of project.
	`

	pathReleaseHelpDesc = `
	Performs release of project by the specified git tag.
	Provided command should prepare release artifacts in the /result directory, which will be published into the TUF repository.
	`
)
