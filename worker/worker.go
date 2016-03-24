package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/garden"
	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/baggageclaim"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
)

var ErrUnsupportedResourceType = errors.New("unsupported resource type")
var ErrIncompatiblePlatform = errors.New("incompatible platform")
var ErrMismatchedTags = errors.New("mismatched tags")
var ErrNoVolumeManager = errors.New("worker does not support volume management")

const containerKeepalive = 30 * time.Second
const containerTTL = 5 * time.Minute
const VolumeTTL = containerTTL

const ephemeralPropertyName = "concourse:ephemeral"
const volumePropertyName = "concourse:volumes"
const volumeMountsPropertyName = "concourse:volume-mounts"
const userPropertyName = "user"

//go:generate counterfeiter . Worker

type Worker interface {
	Client

	ActiveContainers() int

	Description() string
	Name() string
}

//go:generate counterfeiter . GardenWorkerDB

type GardenWorkerDB interface {
	CreateContainer(db.Container, time.Duration) (db.SavedContainer, error)
	UpdateExpiresAtOnContainer(handle string, ttl time.Duration) error

	InsertVolume(db.Volume) error
}

type gardenWorker struct {
	gardenClient       garden.Client
	baggageclaimClient baggageclaim.Client
	volumeFactory      VolumeFactory

	imageFetcher ImageFetcher

	db       GardenWorkerDB
	provider WorkerProvider

	clock clock.Clock

	activeContainers int
	resourceTypes    []atc.WorkerResourceType
	platform         string
	tags             atc.Tags
	name             string
}

func NewGardenWorker(
	gardenClient garden.Client,
	baggageclaimClient baggageclaim.Client,
	volumeFactory VolumeFactory,
	imageFetcher ImageFetcher,
	db GardenWorkerDB,
	provider WorkerProvider,
	clock clock.Clock,
	activeContainers int,
	resourceTypes []atc.WorkerResourceType,
	platform string,
	tags atc.Tags,
	name string,
) Worker {
	return &gardenWorker{
		gardenClient:       gardenClient,
		baggageclaimClient: baggageclaimClient,
		volumeFactory:      volumeFactory,
		imageFetcher:       imageFetcher,
		db:                 db,
		provider:           provider,
		clock:              clock,

		activeContainers: activeContainers,
		resourceTypes:    resourceTypes,
		platform:         platform,
		tags:             tags,
		name:             name,
	}
}

func (worker *gardenWorker) CreateVolume(
	logger lager.Logger,
	identifier VolumeIdentifier,
	properties VolumeProperties,
	privileged bool,
	ttl time.Duration,
) (Volume, error) {
	if worker.baggageclaimClient == nil {
		return nil, ErrNoVolumeManager
	}

	bcVolume, err := worker.baggageclaimClient.CreateVolume(
		logger.Session("create-volume"),
		baggageclaim.VolumeSpec{
			Strategy:   baggageclaim.EmptyStrategy{},
			Properties: baggageclaim.VolumeProperties(properties),
			TTL:        ttl,
			Privileged: privileged,
		},
	)
	if err != nil {
		logger.Error("failed-to-create-volume", err)
		return nil, err
	}

	err = worker.db.InsertVolume(db.Volume{
		Handle:     bcVolume.Handle(),
		WorkerName: worker.Name(),
		TTL:        ttl,
		Identifier: db.VolumeIdentifier(identifier),
	})
	if err != nil {
		logger.Error("failed-to-save-volume-to-db", err)
		return nil, err
	}

	volume, found, err := worker.volumeFactory.Build(logger, bcVolume)
	if err != nil {
		logger.Error("failed-build-volume", err)
		return nil, err
	}

	if !found {
		err = ErrMissingVolume
		logger.Error("volume-expired-immediately", err)
		return nil, err
	}

	return volume, nil
}

func (worker *gardenWorker) ListVolumes(logger lager.Logger, properties VolumeProperties) ([]Volume, error) {
	if worker.baggageclaimClient == nil {
		return []Volume{}, nil
	}

	bcVolumes, err := worker.baggageclaimClient.ListVolumes(
		logger,
		baggageclaim.VolumeProperties(properties),
	)
	if err != nil {
		logger.Error("failed-to-list-volumes", err)
		return nil, err
	}

	volumes := []Volume{}
	for _, bcVolume := range bcVolumes {
		volume, found, err := worker.volumeFactory.Build(logger, bcVolume)
		if err != nil {
			return []Volume{}, err
		}

		if !found {
			continue
		}

		volumes = append(volumes, volume)
	}

	return volumes, nil
}

func (worker *gardenWorker) LookupVolume(logger lager.Logger, handle string) (Volume, bool, error) {
	if worker.baggageclaimClient == nil {
		return nil, false, nil
	}

	bcVolume, found, err := worker.baggageclaimClient.LookupVolume(logger, handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	return worker.volumeFactory.Build(logger, bcVolume)
}

func (worker *gardenWorker) CreateContainer(
	logger lager.Logger,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	id Identifier,
	metadata Metadata,
	spec ContainerSpec,
	resourceTypes atc.ResourceTypes,
) (Container, error) {
	var (
		volumeHandles []string
		volumeMounts  []VolumeMount
		gardenSpec    garden.ContainerSpec
		imageFetched  bool
		image         Image
	)
	volumeMountPaths := map[baggageclaim.Volume]string{}

dance:
	switch s := spec.(type) {
	case ResourceTypeContainerSpec:
		if len(s.Mounts) > 0 && s.Cache.Volume != nil {
			return nil, errors.New("a container may not have mounts and a cache")
		}

		volumeMounts = s.Mounts

		if s.Cache.Volume != nil && s.Cache.MountPath != "" {
			volumeHandles = append(volumeHandles, s.Cache.Volume.Handle())
			volumeMountPaths[s.Cache.Volume] = s.Cache.MountPath
		}

		for _, resourceType := range resourceTypes {
			if resourceType.Name == s.Type {
				resourceTypes = resourceTypes.Without(s.Type)
				s.ImageResourcePointer = &atc.TaskImageConfig{
					Source: resourceType.Source,
					Type:   resourceType.Type,
				}
			}
		}

		var err error
		gardenSpec, imageFetched, image, err = worker.baseGardenSpec(
			logger,
			s.ImageResourcePointer,
			worker.tags,
			cancel,
			delegate,
			id,
			metadata,
			worker,
			resourceTypes,
		)
		if err != nil {
			return nil, err
		}

		if imageFetched {
			// ensure the image is released even if the resourceType is invalid
			defer image.Release(nil)
		}

		gardenSpec.Privileged = true
		gardenSpec.Env = append(gardenSpec.Env, s.Env...)

		if s.Ephemeral {
			gardenSpec.Properties[ephemeralPropertyName] = "true"
		}

		if s.ImageResourcePointer == nil {
			for _, t := range worker.resourceTypes {
				if t.Type == s.Type {
					gardenSpec.RootFSPath = t.Image
					break dance
				}
			}

			return nil, ErrUnsupportedResourceType
		}
	case TaskContainerSpec:
		volumeMounts = s.Inputs

		for _, mount := range s.Outputs {
			volume := mount.Volume
			volumeHandles = append(volumeHandles, volume.Handle())
			volumeMountPaths[volume] = mount.MountPath
		}

		var err error
		gardenSpec, imageFetched, image, err = worker.baseGardenSpec(
			logger,
			s.ImageResourcePointer,
			worker.tags,
			cancel,
			delegate,
			id,
			metadata,
			worker,
			resourceTypes,
		)
		if err != nil {
			return nil, err
		}

		if imageFetched {
			defer image.Release(nil)
		}

		gardenSpec.Privileged = s.Privileged

		if s.ImageResourcePointer == nil {
			gardenSpec.RootFSPath = s.Image
		}
	default:
		return nil, fmt.Errorf("unknown container spec type: %T (%#v)", s, s)
	}

	if imageFetched {
		volumeHandles = append(volumeHandles, image.Volume().Handle())
		gardenSpec.Properties[userPropertyName] = image.Metadata().User
	} else {
		gardenSpec.Properties[userPropertyName] = ""
	}

	for _, mount := range volumeMounts {
		cowBCVolume, err := worker.baggageclaimClient.CreateVolume(logger, baggageclaim.VolumeSpec{
			Strategy: baggageclaim.COWStrategy{
				Parent: mount.Volume,
			},
			Privileged: gardenSpec.Privileged,
			TTL:        VolumeTTL,
		})
		if err != nil {
			return nil, err
		}

		err = worker.db.InsertVolume(db.Volume{
			Handle:     cowBCVolume.Handle(),
			WorkerName: worker.Name(),
			TTL:        VolumeTTL,
			Identifier: db.VolumeIdentifier{
				COW: &db.COWIdentifier{
					ParentVolumeHandle: mount.Volume.Handle(),
				},
			},
		})
		if err != nil {
			return nil, err
		}

		cowVolume, found, err := worker.volumeFactory.Build(logger, cowBCVolume)
		if err != nil {
			return nil, err
		}

		if !found {
			err = ErrMissingVolume
			logger.Error("cow-volume-expired-immediately", err)
			return nil, err
		}

		// release *after* container creation
		defer cowVolume.Release(nil)

		volumeHandles = append(volumeHandles, cowVolume.Handle())
		volumeMountPaths[cowVolume] = mount.MountPath

		logger.Debug("created-cow-volume", lager.Data{
			"original-volume-handle": mount.Volume.Handle(),
			"cow-volume-handle":      cowVolume.Handle(),
		})
	}

	for volume, mount := range volumeMountPaths {
		gardenSpec.BindMounts = append(gardenSpec.BindMounts, garden.BindMount{
			SrcPath: volume.Path(),
			DstPath: mount,
			Mode:    garden.BindMountModeRW,
		})
	}

	if len(volumeHandles) > 0 {
		volumesJSON, err := json.Marshal(volumeHandles)
		if err != nil {
			return nil, err
		}

		gardenSpec.Properties[volumePropertyName] = string(volumesJSON)

		volumeHandleMounts := map[string]string{}
		for k, v := range volumeMountPaths {
			volumeHandleMounts[k.Handle()] = v
		}

		mountsJSON, err := json.Marshal(volumeHandleMounts)
		if err != nil {
			return nil, err
		}

		gardenSpec.Properties[volumeMountsPropertyName] = string(mountsJSON)
	}

	gardenContainer, err := worker.gardenClient.Create(gardenSpec)
	if err != nil {
		return nil, err
	}

	metadata.WorkerName = worker.name
	metadata.Handle = gardenContainer.Handle()
	metadata.User = gardenSpec.Properties["user"]
	_, err = worker.db.CreateContainer(
		db.Container{
			ContainerIdentifier: db.ContainerIdentifier(id),
			ContainerMetadata:   db.ContainerMetadata(metadata),
		},
		containerTTL,
	)
	if err != nil {
		return nil, err
	}

	return newGardenWorkerContainer(
		logger,
		gardenContainer,
		worker.gardenClient,
		worker.baggageclaimClient,
		worker.db,
		worker.clock,
		worker.volumeFactory,
	)
}

func (worker *gardenWorker) baseGardenSpec(
	logger lager.Logger,
	taskImageConfig *atc.TaskImageConfig,
	workerTags atc.Tags,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	id Identifier,
	metadata Metadata,
	workerClient Client,
	resourceTypes atc.ResourceTypes,
) (garden.ContainerSpec, bool, Image, error) {
	if taskImageConfig != nil {
		image, err := worker.imageFetcher.FetchImage(
			logger,
			*taskImageConfig,
			cancel,
			id,
			metadata,
			delegate,
			workerClient,
			workerTags,
			resourceTypes,
		)
		if err != nil {
			return garden.ContainerSpec{}, false, nil, err
		}

		gardenSpec := garden.ContainerSpec{
			Properties: garden.Properties{},
			RootFSPath: path.Join(image.Volume().Path(), "rootfs"),
			Env:        image.Metadata().Env,
		}

		return gardenSpec, true, image, nil
	}

	gardenSpec := garden.ContainerSpec{
		Properties: garden.Properties{},
	}

	return gardenSpec, false, nil, nil
}

func (worker *gardenWorker) FindContainerForIdentifier(logger lager.Logger, id Identifier) (Container, bool, error) {
	containerInfo, found, err := worker.provider.FindContainerForIdentifier(id)
	if err != nil {
		return nil, false, err
	}

	if !found {
		return nil, found, nil
	}

	container, found, err := worker.LookupContainer(logger, containerInfo.Handle)
	if err != nil {
		return nil, false, err
	}

	if !found {
		logger.Info("reaping-container-not-found-on-worker", lager.Data{
			"container-handle": containerInfo.Handle,
			"worker-name":      containerInfo.WorkerName,
		})

		err := worker.provider.ReapContainer(containerInfo.Handle)
		if err != nil {
			return nil, false, err
		}

		return nil, false, err
	}

	return container, found, nil
}

func (worker *gardenWorker) LookupContainer(logger lager.Logger, handle string) (Container, bool, error) {
	gardenContainer, err := worker.gardenClient.Lookup(handle)
	if err != nil {
		if _, ok := err.(garden.ContainerNotFoundError); ok {
			logger.Info("container-not-found")
			return nil, false, nil
		}

		logger.Error("failed-to-lookup-on-garden", err)
		return nil, false, err
	}

	container, err := newGardenWorkerContainer(
		logger,
		gardenContainer,
		worker.gardenClient,
		worker.baggageclaimClient,
		worker.db,
		worker.clock,
		worker.volumeFactory,
	)
	if err != nil {
		logger.Error("failed-to-construct-container", err)
		return nil, false, err
	}

	return container, true, nil
}

func (worker *gardenWorker) ActiveContainers() int {
	return worker.activeContainers
}

func (worker *gardenWorker) Satisfying(spec WorkerSpec, resourceTypes atc.ResourceTypes) (Worker, error) {
	if spec.ResourceType != "" {
		underlyingType := determineUnderlyingTypeName(spec.ResourceType, resourceTypes)

		matchedType := false
		for _, t := range worker.resourceTypes {
			if t.Type == underlyingType {
				matchedType = true
				break
			}
		}

		if !matchedType {
			return nil, ErrUnsupportedResourceType
		}
	}

	if spec.Platform != "" {
		if spec.Platform != worker.platform {
			return nil, ErrIncompatiblePlatform
		}
	}

	if !worker.tagsMatch(spec.Tags) {
		return nil, ErrMismatchedTags
	}

	return worker, nil
}

func determineUnderlyingTypeName(typeName string, resourceTypes atc.ResourceTypes) string {
	resourceTypesMap := make(map[string]atc.ResourceType)
	for _, resourceType := range resourceTypes {
		resourceTypesMap[resourceType.Name] = resourceType
	}
	underlyingTypeName := typeName
	underlyingType, ok := resourceTypesMap[underlyingTypeName]
	for ok {
		underlyingTypeName = underlyingType.Type
		underlyingType, ok = resourceTypesMap[underlyingTypeName]
		delete(resourceTypesMap, underlyingTypeName)
	}
	return underlyingTypeName
}

func (worker *gardenWorker) AllSatisfying(spec WorkerSpec, resourceTypes atc.ResourceTypes) ([]Worker, error) {
	return nil, errors.New("Not implemented")
}

func (worker *gardenWorker) GetWorker(name string) (Worker, error) {
	return nil, errors.New("Not implemented")
}

func (worker *gardenWorker) Description() string {
	messages := []string{
		fmt.Sprintf("platform '%s'", worker.platform),
	}

	for _, tag := range worker.tags {
		messages = append(messages, fmt.Sprintf("tag '%s'", tag))
	}

	return strings.Join(messages, ", ")
}

func (worker *gardenWorker) Name() string {
	return worker.name
}

func (worker *gardenWorker) tagsMatch(tags []string) bool {
	if len(worker.tags) > 0 && len(tags) == 0 {
		return false
	}

insert_coin:
	for _, stag := range tags {
		for _, wtag := range worker.tags {
			if stag == wtag {
				continue insert_coin
			}
		}

		return false
	}

	return true
}
