package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/masterminds/semver"
	"github.com/sirupsen/logrus"

	"github.com/joshvanl/version-checker/pkg/api"
	"github.com/joshvanl/version-checker/pkg/version/docker"
	"github.com/joshvanl/version-checker/pkg/version/gcr"
	"github.com/joshvanl/version-checker/pkg/version/quay"
)

type VersionGetter struct {
	log *logrus.Entry

	quay   *quay.Client
	docker *docker.Client
	gcr    *gcr.Client

	// cacheTimeout is the amount of time a imageCache item is considered fresh
	// for.
	cacheTimeout time.Duration
	cacheMu      sync.RWMutex
	imageCache   map[string]imageCacheItem
}

type ImageClient interface {
	// IsClient will return true if this client is appropriate for the given
	// image URL.
	IsClient(imageURL string) bool

	// Tags will return the available tags for the given image URL at the remote
	// repository.
	Tags(ctx context.Context, imageURL string) ([]api.ImageTag, error)
}

func New(log *logrus.Entry, cacheTimeout time.Duration) *VersionGetter {
	vg := &VersionGetter{
		log:          log.WithField("module", "version_getter"),
		quay:         quay.New(),
		docker:       docker.New(),
		gcr:          gcr.New(),
		imageCache:   make(map[string]imageCacheItem),
		cacheTimeout: cacheTimeout,
	}

	// Start garbage collector
	go vg.garbageCollect(cacheTimeout / 2)

	return vg
}

// LatestTagFromOImage will return the latest tag given an imageURL, according
// to the given options.
func (v *VersionGetter) LatestTagFromImage(ctx context.Context, opts *api.Options, imageURL string) (*api.ImageTag, error) {
	tags, err := v.allTagsFromImage(ctx, imageURL)
	if err != nil {
		return nil, err
	}

	// If UseSHA then return early
	if opts.UseSHA {
		return latestSHA(tags)
	}

	return latestSemver(opts, tags)
}

// allTagsFromImage will return all available tags from the remote repository
// given an imageURL. It also holds a cache for each imageURL that is
// periodically garbage collected.
func (v *VersionGetter) allTagsFromImage(ctx context.Context, imageURL string) ([]api.ImageTag, error) {
	// Check for cache hit
	if tags, ok := v.tryImageCache(imageURL); ok {
		return tags, nil
	}

	// Cache miss so pull fresh tags
	client := v.clientFromImage(imageURL)

	tags, err := client.Tags(ctx, imageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get tags from remote registry for %q: %s",
			imageURL, err)
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("no tags found for given image URL: %q", imageURL)
	}

	v.log.Debugf("committing image tags: %q", imageURL)

	// Add tags to cache
	v.imageCache[imageURL] = imageCacheItem{
		timestamp: time.Now(),
		tags:      tags,
	}

	return tags, nil
}

// CalculateHashIndex returns a hash index given an imageURL and options.
func CalculateHashIndex(imageURL string, opts *api.Options) (string, error) {
	opsJson, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("failed to marshal options: %s", err)
	}

	hash := fnv.New32()
	if _, err := hash.Write(append(opsJson, []byte(imageURL)...)); err != nil {
		return "", fmt.Errorf("failed to calculate image hash: %s", err)
	}

	return fmt.Sprintf("%d", hash.Sum32()), nil
}

// clientFromImage will return the appropriate registry client for a given
// image URL.
func (v *VersionGetter) clientFromImage(imageURL string) ImageClient {
	switch {
	case v.quay.IsClient(imageURL):
		return v.quay
	case v.gcr.IsClient(imageURL):
		return v.gcr
	case v.docker.IsClient(imageURL):
		return v.docker
	default:
		// Fall back to docker if we can't determine the registry
		return v.docker
	}
}

// latestSemver will return the latest ImageTag based on the given options
// restriction, using semver. This should not be used is UseSHA has been
// enabled.
func latestSemver(opts *api.Options, tags []api.ImageTag) (*api.ImageTag, error) {
	var (
		latestImageTag *api.ImageTag
		latestSemVer   *semver.Version
	)

	for i := range tags {
		v, err := semver.NewVersion(tags[i].Tag)
		if err == semver.ErrInvalidSemVer {
			continue
		}
		if err != nil {
			return nil, err
		}

		// If regex enabled but doesn't match tag, continue
		if opts.RegexMatcher != nil && !opts.RegexMatcher.MatchString(tags[i].Tag) {
			continue
		}

		// Optionally use pre-release
		if v.Prerelease() != "" && !opts.UsePreRelease {
			continue
		}

		if opts.PinMajor != nil && v.Major() != *opts.PinMajor {
			continue
		}
		if opts.PinMinor != nil && v.Minor() != *opts.PinMinor {
			continue
		}
		if opts.PinPatch != nil && v.Patch() != *opts.PinPatch {
			continue
		}

		if latestSemVer == nil || latestSemVer.LessThan(v) {
			latestSemVer = v
			latestImageTag = &tags[i]
		}
	}

	if latestImageTag == nil {
		return nil, fmt.Errorf("no image found with those option constraints: %+v", opts)
	}

	return latestImageTag, nil
}

// latestSHA will return the latest ImageTag based on image timestamps.
func latestSHA(tags []api.ImageTag) (*api.ImageTag, error) {
	var latestTag *api.ImageTag

	for i := range tags {
		if latestTag == nil || tags[i].Timestamp.After(latestTag.Timestamp) {
			latestTag = &tags[i]
		}
	}

	if latestTag == nil {
		return nil, errors.New("failed to find latest image based on SHA")
	}

	return latestTag, nil
}
