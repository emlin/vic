// Copyright 2016-2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backends

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/docker/distribution/digest"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	containertypes "github.com/docker/docker/api/types/container"
	eventtypes "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/builder/dockerfile"
	dockerimage "github.com/docker/docker/image"
	dockerLayer "github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/reference"

	"github.com/vmware/vic/lib/apiservers/engine/backends/cache"
	"github.com/vmware/vic/lib/apiservers/portlayer/models"
	vicarchive "github.com/vmware/vic/lib/archive"
	"github.com/vmware/vic/lib/imagec"
	"github.com/vmware/vic/pkg/trace"
	"github.com/vmware/vic/pkg/version"
	"github.com/vmware/vic/pkg/vsphere/sys"
)

// Commit creates a new filesystem image from the current state of a container.
// The image can optionally be tagged into a repository.
func (i *Image) Commit(name string, config *backend.ContainerCommitConfig) (imageID string, err error) {
	defer trace.End(trace.Begin(name))

	// Look up the container name in the metadata cache to get long ID
	vc := cache.ContainerCache().GetContainer(name)
	if vc == nil {
		return "", NotFoundError(name)
	}

	// get container info
	c, err := containerEngine.ContainerInspect(name, false, "")
	if err != nil {
		return "", InternalServerError(err.Error())
	}
	container, ok := c.(*types.ContainerJSON)
	if !ok {
		return "", InternalServerError(fmt.Sprintf("Container type assertion failed"))
	}
	if container.State.Running || container.State.Restarting {
		return "", ConflictError(fmt.Sprintf("%s does not support commit of a running container", ProductName()))
	}
	// TODO: pause container after container.Pause is implemented
	newConfig, err := dockerfile.BuildFromConfig(config.Config, config.Changes)
	if err != nil {
		return "", err
	}

	if config.MergeConfigs {
		if err := merge(newConfig, container.Config); err != nil {
			return "", err
		}
	}

	filter := vicarchive.FilterSpec{}
	rc, err := containerEngine.containerProxy.ArchiveExportReader(context.Background(), containerStoreName, imageStoreName, vc.ContainerID, vc.LayerID, true, filter)
	if err != nil {
		return "", fmt.Errorf("Unable to initialize export stream reader for container %s", name)
	}

	ic, err := getImagec(config)
	if err != nil {
		return "", err
	}

	lm, err := downloadDiff(rc, container.ID, ic.Options)

	if err = setLayerConfig(lm, container, config, newConfig); err != nil {
		return "", err
	}
	// Dump metadata next to diff file
	destination := path.Join(imagec.DestinationDirectory(ic.Options), lm.ID)
	err = ioutil.WriteFile(path.Join(destination, lm.ID+".json"), []byte(lm.Meta), 0644)
	if err != nil {
		return "", err
	}
	imagec.LayerCache().Add(lm)

	var layers []*imagec.ImageWithMeta

	layers = append(layers, lm)
	for pl := lm.Parent; pl != imagec.ScratchLayerID; {
		// populate manifest layer with existing cached data
		if lm, err = imagec.LayerCache().Get(pl); err != nil {
			return "", InternalServerError(fmt.Sprintf("Failed to get parent image layer %s: %s", pl, err))
		}
		layers = append(layers, lm)
	}

	ic.ImageLayers = layers

	imageConfig, err := ic.CreateImageConfig(layers)
	if err != nil {
		return "", err
	}
	// place calculated ImageID in struct
	ic.ImageID = imageConfig.ImageID

	// cache and persist the image
	cache.ImageCache().Add(&imageConfig)
	if err := cache.ImageCache().Save(); err != nil {
		return "", fmt.Errorf("error saving image cache: %s", err)
	}

	// if repo:tag is specified, update image to repo cache, otherwise, this image will be updated to repo cache while it's tagged
	if ic.Reference != nil {
		imagec.UpdateRepositoryCache(ic)
	}

	// Write blob to the storage layer
	if err = ic.WriteImageBlob(lm, progress.DiscardOutput(), true); err != nil {
		return "", err
	}

	imagec.LayerCache().Commit(lm)

	refName := ""
	if ic.Reference != nil {
		refName = ic.Reference.String()
	}
	actor := CreateImageEventActorWithAttributes(imageConfig.ImageID, refName, map[string]string{})
	EventService().Log("commit", eventtypes.ImageEventType, actor)
	return imageConfig.ImageID, nil
}

func getImagec(config *backend.ContainerCommitConfig) (*imagec.ImageC, error) {
	var imageRef reference.Named
	if config.Repo != "" {
		newTag, err := reference.WithName(config.Repo)
		if err != nil {
			return nil, err
		}
		if config.Tag != "" {
			if newTag, err = reference.WithTag(newTag, config.Tag); err != nil {
				return nil, err
			}
		}
	}
	options := imagec.Options{
		Reference: imageRef,
	}

	ic := imagec.NewImageC(options, streamformatter.NewJSONStreamFormatter())
	if imageRef != nil {
		ic.ParseReference()
	}
	return ic, nil
}

func setLayerConfig(lm *imagec.ImageWithMeta, container *types.ContainerJSON, config *backend.ContainerCommitConfig, newConfig *containertypes.Config) error {
	defer trace.End(trace.Begin(lm.ID))

	// Host is either the host's UUID (if run on vsphere) or the hostname of
	// the system (if run standalone)
	host, err := sys.UUID()
	if err != nil {
		return InternalServerError(fmt.Sprintf("Failed to get host name: %s", err))
	}

	if host != "" {
		log.Infof("Using UUID (%s) for imagestore name", host)
	}

	vc := cache.ContainerCache().GetContainer(container.ID)
	meta := dockerimage.V1Image{
		ID:              lm.ID,
		Parent:          vc.LayerID,
		Comment:         config.Comment,
		Created:         time.Now().UTC(),
		Container:       container.ID,
		ContainerConfig: *container.Config,
		Architecture:    runtime.GOARCH,
		OS:              runtime.GOOS,
		DockerVersion:   version.DockerServerVersion,
		Config:          newConfig,
		Size:            lm.Size,
	}

	m, err := json.Marshal(meta)
	if err != nil {
		return InternalServerError(fmt.Sprintf("Failed to marshal image layer config: %s", err))
	}
	// layer metadata
	lm.Meta = string(m)
	lm.Image = &models.Image{
		ID:     lm.ID,
		Parent: vc.ImageID,
		Store:  host,
	}

	return nil
}

func downloadDiff(rc io.ReadCloser, containerID string, options imagec.Options) (*imagec.ImageWithMeta, error) {
	defer trace.End(trace.Begin(containerID))

	// generate random string as layer ID
	layerID := stringid.GenerateRandomID()

	tmpLayerFileName, diffIDSum, gzSum, err := compressDiffToTmpFile(rc, containerID)
	if err != nil {
		return nil, err
	}

	// Cleanup function for the error case
	defer func() {
		if err != nil {
			os.Remove(tmpLayerFileName)
		}
	}()

	blobSum := digest.NewDigestFromBytes(digest.SHA256, gzSum)
	log.Debugf("container %s blob sum: %s", containerID, blobSum.String())
	diffID := digest.NewDigestFromBytes(digest.SHA256, diffIDSum)
	log.Debugf("container %s diff id: %s", containerID, diffID.String())

	layerFile, err := os.Open(string(tmpLayerFileName))
	if err != nil {
		return nil, err
	}
	defer layerFile.Close()

	decompressed, err := gzip.NewReader(layerFile)
	if err != nil {
		return nil, err
	}
	defer decompressed.Close()

	// get a tar reader
	tr := tar.NewReader(decompressed)

	// iterate through tar headers to get file sizes
	var layerSize int64
	for {
		tarHeader, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			err = terr
			return nil, err
		}
		layerSize += tarHeader.Size
	}

	if layerSize == 0 {
		diffID = digest.Digest(dockerLayer.DigestSHA256EmptyTar)
	}
	log.Debugf("container %s size: %d", containerID, layerSize)

	// Ensure the parent directory exists
	destination := path.Join(imagec.DestinationDirectory(options), layerID)
	err = os.MkdirAll(destination, 0755) /* #nosec */
	if err != nil {
		return nil, err
	}

	// Move(rename) the temporary file to its final destination
	err = os.Rename(string(tmpLayerFileName), path.Join(destination, layerID+".tar"))
	if err != nil {
		return nil, err
	}

	// layer metadata
	lm := &imagec.ImageWithMeta{
		DiffID: diffID.String(),
		Layer: imagec.FSLayer{
			BlobSum: blobSum.String(),
		},
		Size: layerSize,
	}
	return lm, nil
}

// compressDiffToTmpFile will write stream to temp file, and return temp file name and tar file checksum, compressed file checksum
func compressDiffToTmpFile(rc io.ReadCloser, containerID string) (string, []byte, []byte, error) {
	defer trace.End(trace.Begin(containerID))
	// Create a temporary file and stream the res.Body into it
	var out *os.File
	var gzWriter *gzip.Writer
	var err error

	cleanup := func() {
		if gzWriter != nil {
			gzWriter.Close()
			gzWriter = nil
		}
		if out != nil {
			out.Close()
			if err != nil {
				os.Remove(out.Name())
			}
			out = nil
		}
	}
	defer cleanup()

	out, err = ioutil.TempFile("", containerID)
	if err != nil {
		return "", nil, nil, err
	}

	// compress tar file using gzip and calculate blobsum and diffID all together using multi writer
	blobSum := sha256.New()
	diffID := sha256.New()
	compressedMW := io.MultiWriter(out, blobSum)

	gzWriter = gzip.NewWriter(compressedMW)
	tarMW := io.MultiWriter(gzWriter, diffID)
	_, err = io.Copy(tarMW, rc)
	if err != nil {
		log.Errorf("failed to stream to file: %s", err)
		return "", nil, nil, err
	}

	// close writer before calculate checksum
	fileName := out.Name()
	gzWriter.Flush()
	cleanup()
	// Return the temporary file name and checksum
	return fileName, diffID.Sum(nil), blobSum.Sum(nil), nil
}

// ***** Code from Docker v17.03.2-ce PullImage to merge two Configs

// merge merges two Config, the image container configuration (defaults values),
// and the user container configuration, either passed by the API or generated
// by the cli.
// It will mutate the specified user configuration (userConf) with the image
// configuration where the user configuration is incomplete.
func merge(userConf, imageConf *containertypes.Config) error {
	if userConf.User == "" {
		userConf.User = imageConf.User
	}
	if len(userConf.ExposedPorts) == 0 {
		userConf.ExposedPorts = imageConf.ExposedPorts
	} else if imageConf.ExposedPorts != nil {
		for port := range imageConf.ExposedPorts {
			if _, exists := userConf.ExposedPorts[port]; !exists {
				userConf.ExposedPorts[port] = struct{}{}
			}
		}
	}

	if len(userConf.Env) == 0 {
		userConf.Env = imageConf.Env
	} else {
		for _, imageEnv := range imageConf.Env {
			found := false
			imageEnvKey := strings.Split(imageEnv, "=")[0]
			for _, userEnv := range userConf.Env {
				userEnvKey := strings.Split(userEnv, "=")[0]
				if runtime.GOOS == "windows" {
					// Case insensitive environment variables on Windows
					imageEnvKey = strings.ToUpper(imageEnvKey)
					userEnvKey = strings.ToUpper(userEnvKey)
				}
				if imageEnvKey == userEnvKey {
					found = true
					break
				}
			}
			if !found {
				userConf.Env = append(userConf.Env, imageEnv)
			}
		}
	}

	if userConf.Labels == nil {
		userConf.Labels = map[string]string{}
	}
	for l, v := range imageConf.Labels {
		if _, ok := userConf.Labels[l]; !ok {
			userConf.Labels[l] = v
		}
	}

	if len(userConf.Entrypoint) == 0 {
		if len(userConf.Cmd) == 0 {
			userConf.Cmd = imageConf.Cmd
			userConf.ArgsEscaped = imageConf.ArgsEscaped
		}

		if userConf.Entrypoint == nil {
			userConf.Entrypoint = imageConf.Entrypoint
		}
	}
	if imageConf.Healthcheck != nil {
		if userConf.Healthcheck == nil {
			userConf.Healthcheck = imageConf.Healthcheck
		} else {
			if len(userConf.Healthcheck.Test) == 0 {
				userConf.Healthcheck.Test = imageConf.Healthcheck.Test
			}
			if userConf.Healthcheck.Interval == 0 {
				userConf.Healthcheck.Interval = imageConf.Healthcheck.Interval
			}
			if userConf.Healthcheck.Timeout == 0 {
				userConf.Healthcheck.Timeout = imageConf.Healthcheck.Timeout
			}
			if userConf.Healthcheck.Retries == 0 {
				userConf.Healthcheck.Retries = imageConf.Healthcheck.Retries
			}
		}
	}

	if userConf.WorkingDir == "" {
		userConf.WorkingDir = imageConf.WorkingDir
	}
	if len(userConf.Volumes) == 0 {
		userConf.Volumes = imageConf.Volumes
	} else {
		for k, v := range imageConf.Volumes {
			userConf.Volumes[k] = v
		}
	}

	if userConf.StopSignal == "" {
		userConf.StopSignal = imageConf.StopSignal
	}
	return nil
}

// *****
