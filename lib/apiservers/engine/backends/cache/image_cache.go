// Copyright 2016 VMware, Inc. All Rights Reserved.
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

package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"

	"github.com/docker/distribution/digest"
	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/reference"

	"github.com/vmware/vic/lib/apiservers/portlayer/client"
	"github.com/vmware/vic/lib/apiservers/portlayer/client/storage"
	"github.com/vmware/vic/lib/metadata"
	"github.com/vmware/vic/pkg/vsphere/sys"
)

// ICache is an in-memory cache of image metadata. It is refreshed at startup
// by a call to the portlayer. It is updated when new images are pulled or
// images are deleted.
type ICache struct {
	m sync.RWMutex

	// cache maps image ID to image metadata
	idIndex     *truncindex.TruncIndex
	cacheByID   map[string]*metadata.ImageConfig
	cacheByName map[string]*metadata.ImageConfig
}

var (
	imageCache *ICache
	ctx        = context.TODO()
)

func init() {
	imageCache = &ICache{
		idIndex:     truncindex.NewTruncIndex([]string{}),
		cacheByID:   make(map[string]*metadata.ImageConfig),
		cacheByName: make(map[string]*metadata.ImageConfig),
	}
}

// ImageCache returns a reference to the image cache
func ImageCache() *ICache {
	return imageCache
}

// Update runs only once at startup to hydrate the image cache
func (ic *ICache) Update(client *client.PortLayer) error {
	log.Debugf("Updating image cache")

	host, err := sys.UUID()
	if host == "" {
		host, err = os.Hostname()
	}
	if err != nil {
		return fmt.Errorf("Unexpected error getting hostname: %s", err)
	}

	params := storage.NewListImagesParamsWithContext(ctx).WithStoreName(host)

	layers, err := client.Storage.ListImages(params)
	if err != nil {
		return fmt.Errorf("Failed to retrieve image list from portlayer: %s", err)
	}

	for _, layer := range layers.Payload {

		// populate the layer cache as we go
		// TODO(jzt): this will probably change once the k/v store is being used to track
		// images (and layers?)
		LayerCache().AddExisting(layer.ID)

		imageConfig := &metadata.ImageConfig{}
		if err := json.Unmarshal([]byte(layer.Metadata[metadata.MetaDataKey]), imageConfig); err != nil {
			derr.NewErrorWithStatusCode(fmt.Errorf("Failed to unmarshal image config: %s", err),
				http.StatusInternalServerError)
		}

		if imageConfig.ImageID != "" {
			ic.AddImage(imageConfig)
		}
	}

	return nil
}

// GetImages returns a slice containing metadata for all cached images
func (ic *ICache) GetImages() []*metadata.ImageConfig {
	ic.m.RLock()
	defer ic.m.RUnlock()

	result := make([]*metadata.ImageConfig, 0, len(ic.cacheByID))
	for _, image := range ic.cacheByID {
		result = append(result, copyImageConfig(image))
	}
	return result
}

// IsImageID will check that a full or partial imageID
// exists in the cache
func (ic *ICache) IsImageID(id string) bool {
	ic.m.RLock()
	defer ic.m.RUnlock()
	if _, err := ic.idIndex.Get(id); err == nil {
		return true
	}
	return false
}

// GetImage parses input to retrieve a cached image
func (ic *ICache) GetImage(idOrRef string) (*metadata.ImageConfig, error) {
	ic.m.RLock()
	defer ic.m.RUnlock()

	// cover the case of creating by a full reference
	if config, ok := ic.cacheByName[idOrRef]; ok {
		return config, nil
	}

	// get the full image ID if supplied a prefix
	if id, err := ic.idIndex.Get(idOrRef); err == nil {
		idOrRef = id
	}

	imgDigest, named, err := reference.ParseIDOrReference(idOrRef)
	if err != nil {
		return nil, err
	}

	var config *metadata.ImageConfig
	if imgDigest != "" {
		config = ic.getImageByDigest(imgDigest)
	} else {
		config = ic.getImageByNamed(named)
	}

	if config == nil {
		// docker automatically prints out ":latest" tag if not specified in case if image is not found.
		postfixLatest := ""
		if !strings.Contains(idOrRef, ":") {
			postfixLatest += ":" + reference.DefaultTag
		}
		return nil, derr.NewRequestNotFoundError(fmt.Errorf(
			"No such image: %s%s", idOrRef, postfixLatest))
	}

	return copyImageConfig(config), nil
}

func (ic *ICache) getImageByDigest(digest digest.Digest) *metadata.ImageConfig {
	var config *metadata.ImageConfig
	config, ok := ic.cacheByID[string(digest)]
	if !ok {
		return nil
	}
	return copyImageConfig(config)
}

// Looks up image by reference.Named
func (ic *ICache) getImageByNamed(named reference.Named) *metadata.ImageConfig {
	// get the imageID from the repoCache
	id, _ := RepositoryCache().Get(named)
	return copyImageConfig(ic.cacheByID[prefixImageID(id)])
}

// Add the "sha256:" prefix to the image ID if missing.
// Don't assume the image id in image has "sha256:<id> as format.  We store it in
// this format to make it easier to lookup by digest
func prefixImageID(imageID string) string {
	if strings.HasPrefix(imageID, "sha256:") {
		return imageID
	}
	return "sha256:" + imageID
}

// AddImage adds an image to the image cache
func (ic *ICache) AddImage(imageConfig *metadata.ImageConfig) {

	ic.m.Lock()
	defer ic.m.Unlock()

	// Normalize the name stored in imageConfig using Docker's reference code
	ref, err := reference.WithName(imageConfig.Name)
	if err != nil {
		log.Errorf("Tried to create reference from %s: %s", imageConfig.Name, err.Error())
		return
	}

	imageID := prefixImageID(imageConfig.ImageID)
	ic.idIndex.Add(imageConfig.ImageID)
	ic.cacheByID[imageID] = imageConfig

	for _, tag := range imageConfig.Tags {
		ref, err = reference.WithTag(ref, tag)
		if err != nil {
			log.Errorf("Tried to create tagged reference from %s and tag %s: %s", imageConfig.Name, tag, err.Error())
			return
		}
		ic.cacheByName[imageConfig.Reference] = imageConfig
	}
}

// RemoveImageByConfig removes image from the cache.
func (ic *ICache) RemoveImageByConfig(imageConfig *metadata.ImageConfig) {
	ic.m.Lock()
	defer ic.m.Unlock()

	// If we get here we definitely want to remove image config from any data structure
	// where it can be present. So that, if there is something is wrong
	// it could be tracked on debug level.
	if err := ic.idIndex.Delete(imageConfig.ImageID); err != nil {
		log.Debugf("Not found in image cache index: %v", err)
	}

	prefixedID := prefixImageID(imageConfig.ImageID)
	if _, ok := ic.cacheByID[prefixedID]; ok {
		delete(ic.cacheByID, prefixedID)
	} else {
		log.Debugf("Not found in cache by id: %s", prefixedID)
	}

	if _, ok := ic.cacheByName[imageConfig.Reference]; ok {
		delete(ic.cacheByName, imageConfig.Reference)
	} else {
		log.Debugf("Not found in cache by name: %s", imageConfig.Reference)
	}
}

// copyImageConfig performs and returns deep copy of an ImageConfig struct
func copyImageConfig(image *metadata.ImageConfig) *metadata.ImageConfig {

	if image == nil {
		return nil
	}

	// copy everything
	newImage := *image

	// replace the pointer to metadata.ImageConfig.Config and copy the contents
	newConfig := *image.Config
	newImage.Config = &newConfig

	// add tags & digests from the repo cache
	newImage.Tags = RepositoryCache().Tags(newImage.ImageID)
	newImage.Digests = RepositoryCache().Digests(newImage.ImageID)

	return &newImage
}