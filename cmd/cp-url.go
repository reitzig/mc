// Copyright (c) 2015-2022 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/mc/pkg/probe"
)

type copyURLsType uint8

//   NOTE: All the parse rules should reduced to A: Copy(Source, Target).
//
//   * VALID RULES
//   =======================
//   A: copy(f, f) -> copy(f, f)
//   B: copy(f, d) -> copy(f, d/f) -> []A
//   C: copy(d1..., d2) -> []copy(f, d2/d1/f) -> []A
//   D: copy([]f, d) -> []B

//   * INVALID RULES
//   =========================
//   copy(d, f)
//   copy(d..., f)
//   copy([](f|d)..., f)

const (
	copyURLsTypeInvalid copyURLsType = iota
	copyURLsTypeA
	copyURLsTypeB
	copyURLsTypeC
	copyURLsTypeD
)

// guessCopyURLType guesses the type of clientURL. This approach all allows prepareURL
// functions to accurately report failure causes.
func guessCopyURLType(ctx context.Context, o prepareCopyURLsOpts) (copyURLsType, string, *probe.Error) {
	if len(o.sourceURLs) == 1 { // 1 Source, 1 Target
		var err *probe.Error
		var sourceContent *ClientContent
		sourceURL := o.sourceURLs[0]
		if !o.isRecursive {
			_, sourceContent, err = url2Stat(ctx, sourceURL, o.versionID, false, o.encKeyDB, o.timeRef, o.isZip)
		} else {
			_, sourceContent, err = firstURL2Stat(ctx, sourceURL, o.timeRef, o.isZip)
		}
		if err != nil {
			return copyURLsTypeInvalid, "", err
		}

		// If recursion is ON, it is type C.
		// If source is a folder, it is Type C.
		if sourceContent.Type.IsDir() || o.isRecursive {
			return copyURLsTypeC, "", nil
		}

		// If target is a folder, it is Type B.
		if isAliasURLDir(ctx, o.targetURL, o.encKeyDB, o.timeRef) {
			return copyURLsTypeB, sourceContent.VersionID, nil
		}
		// else Type A.
		return copyURLsTypeA, sourceContent.VersionID, nil
	}

	// Multiple source args and target is a folder. It is Type D.
	if isAliasURLDir(ctx, o.targetURL, o.encKeyDB, o.timeRef) {
		return copyURLsTypeD, "", nil
	}

	return copyURLsTypeInvalid, "", errInvalidArgument().Trace()
}

// SINGLE SOURCE - Type A: copy(f, f) -> copy(f, f)
// prepareCopyURLsTypeA - prepares target and source clientURLs for copying.
func prepareCopyURLsTypeA(ctx context.Context, sourceURL, sourceVersion, targetURL string, encKeyDB map[string][]prefixSSEPair, isZip bool) URLs {
	// Extract alias before fiddling with the clientURL.
	sourceAlias, _, _ := mustExpandAlias(sourceURL)
	// Find alias and expanded clientURL.
	targetAlias, targetURL, _ := mustExpandAlias(targetURL)

	_, sourceContent, err := url2Stat(ctx, sourceURL, sourceVersion, false, encKeyDB, time.Time{}, isZip)
	if err != nil {
		// Source does not exist or insufficient privileges.
		return URLs{Error: err.Trace(sourceURL)}
	}
	if !sourceContent.Type.IsRegular() {
		// Source is not a regular file
		return URLs{Error: errInvalidSource(sourceURL).Trace(sourceURL)}
	}

	// All OK.. We can proceed. Type A
	return makeCopyContentTypeA(sourceAlias, sourceContent, targetAlias, targetURL)
}

// prepareCopyContentTypeA - makes CopyURLs content for copying.
func makeCopyContentTypeA(sourceAlias string, sourceContent *ClientContent, targetAlias, targetURL string) URLs {
	targetContent := ClientContent{URL: *newClientURL(targetURL)}
	return URLs{
		SourceAlias:   sourceAlias,
		SourceContent: sourceContent,
		TargetAlias:   targetAlias,
		TargetContent: &targetContent,
	}
}

// SINGLE SOURCE - Type B: copy(f, d) -> copy(f, d/f) -> A
// prepareCopyURLsTypeB - prepares target and source clientURLs for copying.
func prepareCopyURLsTypeB(ctx context.Context, sourceURL, sourceVersion, targetURL string, encKeyDB map[string][]prefixSSEPair, isZip bool) URLs {
	// Extract alias before fiddling with the clientURL.
	sourceAlias, _, _ := mustExpandAlias(sourceURL)
	// Find alias and expanded clientURL.
	targetAlias, targetURL, _ := mustExpandAlias(targetURL)

	_, sourceContent, err := url2Stat(ctx, sourceURL, sourceVersion, false, encKeyDB, time.Time{}, isZip)
	if err != nil {
		// Source does not exist or insufficient privileges.
		return URLs{Error: err.Trace(sourceURL)}
	}

	if !sourceContent.Type.IsRegular() {
		if sourceContent.Type.IsDir() {
			return URLs{Error: errSourceIsDir(sourceURL).Trace(sourceURL)}
		}
		// Source is not a regular file.
		return URLs{Error: errInvalidSource(sourceURL).Trace(sourceURL)}
	}

	// All OK.. We can proceed. Type B: source is a file, target is a folder and exists.
	return makeCopyContentTypeB(sourceAlias, sourceContent, targetAlias, targetURL)
}

// makeCopyContentTypeB - CopyURLs content for copying.
func makeCopyContentTypeB(sourceAlias string, sourceContent *ClientContent, targetAlias, targetURL string) URLs {
	// All OK.. We can proceed. Type B: source is a file, target is a folder and exists.
	targetURLParse := newClientURL(targetURL)
	targetURLParse.Path = filepath.ToSlash(filepath.Join(targetURLParse.Path, filepath.Base(sourceContent.URL.Path)))
	return makeCopyContentTypeA(sourceAlias, sourceContent, targetAlias, targetURLParse.String())
}

// SINGLE SOURCE - Type C: copy(d1..., d2) -> []copy(d1/f, d1/d2/f) -> []A
// prepareCopyRecursiveURLTypeC - prepares target and source clientURLs for copying.
func prepareCopyURLsTypeC(ctx context.Context, sourceURL, targetURL string, isRecursive, isZip bool, timeRef time.Time) <-chan URLs {
	// Extract alias before fiddling with the clientURL.
	sourceAlias, _, _ := mustExpandAlias(sourceURL)
	// Find alias and expanded clientURL.
	targetAlias, targetURL, _ := mustExpandAlias(targetURL)
	copyURLsCh := make(chan URLs)
	go func(sourceURL, targetURL string, copyURLsCh chan URLs) {
		defer close(copyURLsCh)
		sourceClient, err := newClient(sourceURL)
		if err != nil {
			// Source initialization failed.
			copyURLsCh <- URLs{Error: err.Trace(sourceURL)}
			return
		}

		for sourceContent := range sourceClient.List(ctx, ListOptions{Recursive: isRecursive, TimeRef: timeRef, ShowDir: DirNone, ListZip: isZip}) {
			if sourceContent.Err != nil {
				// Listing failed.
				copyURLsCh <- URLs{Error: sourceContent.Err.Trace(sourceClient.GetURL().String())}
				continue
			}

			if !sourceContent.Type.IsRegular() {
				// Source is not a regular file. Skip it for copy.
				continue
			}

			// All OK.. We can proceed. Type B: source is a file, target is a folder and exists.
			copyURLsCh <- makeCopyContentTypeC(sourceAlias, sourceClient.GetURL(), sourceContent, targetAlias, targetURL)
		}
	}(sourceURL, targetURL, copyURLsCh)
	return copyURLsCh
}

// makeCopyContentTypeC - CopyURLs content for copying.
func makeCopyContentTypeC(sourceAlias string, sourceURL ClientURL, sourceContent *ClientContent, targetAlias, targetURL string) URLs {
	newSourceURL := sourceContent.URL
	pathSeparatorIndex := strings.LastIndex(sourceURL.Path, string(sourceURL.Separator))
	newSourceSuffix := filepath.ToSlash(newSourceURL.Path)
	if pathSeparatorIndex > 1 {
		sourcePrefix := filepath.ToSlash(sourceURL.Path[:pathSeparatorIndex])
		newSourceSuffix = strings.TrimPrefix(newSourceSuffix, sourcePrefix)
	}
	newTargetURL := urlJoinPath(targetURL, newSourceSuffix)
	return makeCopyContentTypeA(sourceAlias, sourceContent, targetAlias, newTargetURL)
}

// MULTI-SOURCE - Type D: copy([](f|d...), d) -> []B
// prepareCopyURLsTypeE - prepares target and source clientURLs for copying.
func prepareCopyURLsTypeD(ctx context.Context, sourceURLs []string, targetURL string, isRecursive bool, timeRef time.Time) <-chan URLs {
	copyURLsCh := make(chan URLs)
	go func(sourceURLs []string, targetURL string, copyURLsCh chan URLs) {
		defer close(copyURLsCh)
		for _, sourceURL := range sourceURLs {
			for cpURLs := range prepareCopyURLsTypeC(ctx, sourceURL, targetURL, isRecursive, false, timeRef) {
				copyURLsCh <- cpURLs
			}
		}
	}(sourceURLs, targetURL, copyURLsCh)
	return copyURLsCh
}

type prepareCopyURLsOpts struct {
	sourceURLs           []string
	targetURL            string
	isRecursive          bool
	encKeyDB             map[string][]prefixSSEPair
	olderThan, newerThan string
	timeRef              time.Time
	versionID            string
	isZip                bool
}

// prepareCopyURLs - prepares target and source clientURLs for copying.
func prepareCopyURLs(ctx context.Context, o prepareCopyURLsOpts) chan URLs {
	copyURLsCh := make(chan URLs)
	go func(o prepareCopyURLsOpts) {
		defer close(copyURLsCh)
		cpType, cpVersion, err := guessCopyURLType(ctx, o)
		fatalIf(err.Trace(), "Unable to guess the type of copy operation.")

		switch cpType {
		case copyURLsTypeA:
			copyURLsCh <- prepareCopyURLsTypeA(ctx, o.sourceURLs[0], cpVersion, o.targetURL, o.encKeyDB, o.isZip)
		case copyURLsTypeB:
			copyURLsCh <- prepareCopyURLsTypeB(ctx, o.sourceURLs[0], cpVersion, o.targetURL, o.encKeyDB, o.isZip)
		case copyURLsTypeC:
			for cURLs := range prepareCopyURLsTypeC(ctx, o.sourceURLs[0], o.targetURL, o.isRecursive, o.isZip, o.timeRef) {
				copyURLsCh <- cURLs
			}
		case copyURLsTypeD:
			for cURLs := range prepareCopyURLsTypeD(ctx, o.sourceURLs, o.targetURL, o.isRecursive, o.timeRef) {
				copyURLsCh <- cURLs
			}
		default:
			copyURLsCh <- URLs{Error: errInvalidArgument().Trace(o.sourceURLs...)}
		}
	}(o)

	finalCopyURLsCh := make(chan URLs)
	go func() {
		defer close(finalCopyURLsCh)
		for cpURLs := range copyURLsCh {
			// Skip objects older than --older-than parameter if specified
			if o.olderThan != "" && isOlder(cpURLs.SourceContent.Time, o.olderThan) {
				continue
			}

			// Skip objects newer than --newer-than parameter if specified
			if o.newerThan != "" && isNewer(cpURLs.SourceContent.Time, o.newerThan) {
				continue
			}

			finalCopyURLsCh <- cpURLs
		}
	}()

	return finalCopyURLsCh
}
