package gcng

import (
	"code.cloudfoundry.org/lager"
	"github.com/concourse/atc/dbng"
)

type resourceCacheUseCollector struct {
	logger       lager.Logger
	cacheFactory dbng.ResourceCacheFactory
}

func NewResourceCacheUseCollector(
	logger lager.Logger,
	cacheFactory dbng.ResourceCacheFactory,
) Collector {
	return &resourceCacheUseCollector{
		logger:       logger.Session("resource-cache-use-collector"),
		cacheFactory: cacheFactory,
	}
}

func (rcuc *resourceCacheUseCollector) Run() error {
	err := rcuc.cacheFactory.CleanUsesForFinishedBuilds()
	if err != nil {
		rcuc.logger.Error("unable-to-clean-up-for-builds", err)
		return err
	}

	err = rcuc.cacheFactory.CleanUsesForInactiveResourceTypes()
	if err != nil {
		rcuc.logger.Error("unable-to-clean-up-for-types", err)
		return err
	}

	err = rcuc.cacheFactory.CleanUsesForInactiveResources()
	if err != nil {
		rcuc.logger.Error("unable-to-clean-up-for-resources", err)
		return err
	}

	err = rcuc.cacheFactory.CleanUsesForPausedPipelineResources()
	if err != nil {
		rcuc.logger.Error("unable-to-clean-up-for-paused-pipeline-resources", err)
		return err
	}

	return nil
}
