// Copyright 2022-2023 The Parca Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package debuginfo

import (
	"bufio"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	burrow "github.com/goburrow/cache"
	debuginfopb "github.com/parca-dev/parca/gen/proto/go/parca/debuginfo/v1alpha1"
	parcadebuginfo "github.com/parca-dev/parca/pkg/debuginfo"
	"github.com/parca-dev/parca/pkg/hash"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/parca-dev/parca-agent/pkg/cache"
	"github.com/parca-dev/parca-agent/pkg/objectfile"
)

// Manager is a mechanism for extracting or finding the relevant debug information for the discovered executables.
type Manager struct {
	logger  log.Logger
	tracer  trace.Tracer
	metrics *metrics

	objFilePool *objectfile.Pool

	debuginfoClient debuginfopb.DebuginfoServiceClient
	stripDebuginfos bool
	tempDir         string

	// If requested buildID is not in the cache, we do NOT initiate an upload request to the server.
	shouldInitiateCache burrow.Cache

	// hashCacheKey is used as cache key for all the caches below.
	// hashCache caches ELF hashes.
	hashCache           burrow.Cache
	extractSingleflight *singleflight.Group

	// Makes sure we do not try to upload the same buildID simultaneously.
	uploadSingleflight    *singleflight.Group
	uploadTimeoutDuration time.Duration
	uploadTaskTokens      *semaphore.Weighted

	*Extractor
	*Finder
}

// New creates a new Manager.
func New(
	logger log.Logger,
	tracer trace.Tracer,
	reg prometheus.Registerer,
	objFilePool *objectfile.Pool,
	debuginfoClient debuginfopb.DebuginfoServiceClient,
	uploadMaxParallel int,
	uploadTimeout time.Duration,
	cacheDisabled bool,
	cacheTTL time.Duration,
	debugDirs []string,
	stripDebuginfos bool,
	tempDir string,
) *Manager {
	var (
		shouldInitiateCache burrow.Cache = cache.NewNoopCache()
		hashCache           burrow.Cache = cache.NewNoopCache()
	)
	if !cacheDisabled {
		shouldInitiateCache = burrow.New(
			burrow.WithExpireAfterWrite(cacheTTL),
			burrow.WithStatsCounter(cache.NewBurrowStatsCounter(logger, reg, "debuginfo_should_initiate")),
		)
		hashCache = burrow.New(
			burrow.WithExpireAfterAccess(5*time.Minute),
			burrow.WithStatsCounter(cache.NewBurrowStatsCounter(logger, reg, "debuginfo_hash")),
		)
	}
	return &Manager{
		logger:      logger,
		tracer:      tracer,
		metrics:     newMetrics(reg),
		objFilePool: objFilePool,

		debuginfoClient: debuginfoClient,
		stripDebuginfos: stripDebuginfos,
		tempDir:         tempDir,

		Finder:    NewFinder(logger, tracer, reg, debugDirs),
		Extractor: NewExtractor(logger, tracer),

		shouldInitiateCache: shouldInitiateCache,

		hashCache:           hashCache,
		extractSingleflight: &singleflight.Group{},

		uploadSingleflight:    &singleflight.Group{},
		uploadTimeoutDuration: uploadTimeout,
		uploadTaskTokens:      semaphore.NewWeighted(int64(uploadMaxParallel)),
	}
}

// hashCacheKey is a cache key to retrieve the hashes of debuginfo files.
// Caching reduces allocs by 7.22% (33 kB/operation less) in Upload,
// and it shaves 4 allocs per operation.
type hashCacheKey struct {
	buildID string
	modtime int64
}

// EnsureUploaded ensures that the debuginfo file associated (found or extracted) with the given object file has been uploaded to the server.
// If the debuginfo file has not been uploaded yet, it will be uploaded.
func (di *Manager) EnsureUploaded(ctx context.Context, root string, src *objectfile.ObjectFile) (err error) { //nolint:nonamedreturns
	defer src.HoldOn()

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.EnsureUploaded")
	defer span.End()

	// All the caches and references are based on the source file's buildID.
	if shouldInitiateUpload := di.shouldInitiate(ctx, src.BuildID, src.Path); !shouldInitiateUpload {
		di.metrics.ensureUploadedRequests.WithLabelValues(lvShared).Inc()
		return nil
	}

	defer func() {
		if err != nil {
			di.metrics.ensureUploadedRequests.WithLabelValues(lvFail).Inc()
			span.RecordError(err)
			return
		}
		di.metrics.ensureUploadedRequests.WithLabelValues(lvSuccess).Inc()
	}()

	var dbg *objectfile.ObjectFile
	if src.DebugFile != nil {
		// If the debuginfo file is already extracted or found, we do not need to do it again.
		// We just need to make sure it is uploaded.
		dbg = src.DebugFile

		defer dbg.HoldOn()
	} else {
		// We upload the debug information files asynchronous and concurrently with retry.
		// However, first we need to find the debuginfo file or extract debuginfo from the executable.
		// For the short-lived processes, we may not complete the operation before the process exits.
		// Therefore, to be able shorten this window as much as possible, we extract and find the debuginfo
		// files synchronously and upload them asynchronously.
		// We still might be too slow to obtain the necessary file descriptors for certain short-lived processes.
		if dbg, err = di.ExtractOrFind(ctx, root, src); err != nil {
			di.metrics.ensureUploadedErrors.WithLabelValues(lvExtractOrFind).Inc()
			return err
		}
		defer dbg.HoldOn()
		src.DebugFile = dbg
	}

	// NOTICE: All the caches and references are based on the source file's buildID.
	// Extraction won't change the buildID, but finding might.
	// This is not a problem, Find has its own cache and it will be bounced back from server upload step.
	if err := di.Upload(ctx, dbg); err != nil {
		di.metrics.ensureUploadedErrors.WithLabelValues(lvUpload).Inc()
		return err
	}
	return nil
}

// shouldInitiate checks whether the debuginfo file associated with the given buildID should be uploaded.
// If the buildID is already in the cache, there is no need to extract, find or upload the debuginfo file.
func (di *Manager) shouldInitiate(ctx context.Context, buildID, filepath string) bool {
	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.shouldInitiate")
	defer span.End()

	if _, ok := di.shouldInitiateCache.GetIfPresent(buildID); ok {
		return false
	}

	shouldInitiateResp, err := di.debuginfoClient.ShouldInitiateUpload(ctx, &debuginfopb.ShouldInitiateUploadRequest{
		BuildId: buildID,
	})
	if err != nil {
		level.Error(di.logger).Log("msg", "failed to check whether build ID symbol exists", "err", err, "buildid", buildID, "filepath", filepath)
		span.RecordError(err)
	} else {
		if !shouldInitiateResp.ShouldInitiateUpload {
			di.shouldInitiateCache.Put(buildID, struct{}{})
			return false
		}

		return true
	}

	return true
}

// ExtractOrFind extracts or finds the debug information for the given object file.
// And sets the debuginfo file pointer to the debuginfo object file.
func (di *Manager) ExtractOrFind(ctx context.Context, root string, src *objectfile.ObjectFile) (*objectfile.ObjectFile, error) {
	defer src.HoldOn()

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.ExtractOrFind")
	defer span.End()

	// First, check whether debuginfos have been installed separately,
	// typically in /usr/lib/debug, so we try to discover if there is a debuginfo file,
	// that has the same build ID as the object.
	now := time.Now()
	dbgInfoPath, err := di.Finder.Find(ctx, root, src)
	if err == nil && dbgInfoPath != "" {
		di.metrics.found.WithLabelValues(lvSuccess).Inc()
		di.metrics.findDuration.Observe(time.Since(now).Seconds())
		dbgInfoFile, err := di.objFilePool.Open(dbgInfoPath)
		if err == nil {
			return dbgInfoFile, nil
		}
		defer dbgInfoFile.HoldOn()

		level.Debug(di.logger).Log("msg", "failed to open debuginfo file", "path", dbgInfoPath, "err", err)
	} else {
		di.metrics.found.WithLabelValues(lvFail).Inc()
	}

	// If we didn't find an external debuginfo file, we continue with striping to create one.
	dbgInfoFile, err := di.Extract(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("failed to strip debuginfo: %w", err)
	}
	defer dbgInfoFile.HoldOn()

	return dbgInfoFile, nil
}

func (di *Manager) Extract(ctx context.Context, src *objectfile.ObjectFile) (*objectfile.ObjectFile, error) {
	defer src.HoldOn()

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.Extract")
	defer span.End()

	buildID := src.BuildID

	ef, release, err := src.ELF()
	if err != nil {
		return nil, fmt.Errorf("failed to get ELF file: %w", err)
	}
	defer release()
	binaryHasTextSection := hasTextSection(ef)

	// Only strip the `.text` section if it's present *and* stripping is enabled.
	if di.stripDebuginfos && binaryHasTextSection {
		val, err, shared := di.extractSingleflight.Do(buildID, func() (interface{}, error) {
			return di.extract(ctx, buildID, src)
		})
		if err != nil {
			if shared {
				di.extractSingleflight.Forget(buildID)
			}
			return nil, err
		}

		dbg, ok := val.(*objectfile.ObjectFile)
		if !ok {
			return nil, fmt.Errorf("unexpected type returned: %T", val)
		}
		return dbg, err
	}

	return src, nil
}

func (di *Manager) extract(ctx context.Context, buildID string, src *objectfile.ObjectFile) (_ *objectfile.ObjectFile, err error) { //nolint:nonamedreturns
	defer src.HoldOn()

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.extract")
	defer span.End()

	now := time.Now()
	defer func() {
		if err != nil {
			di.metrics.extracted.WithLabelValues(lvFail).Inc()
			span.RecordError(err)
			return
		}
		di.metrics.extracted.WithLabelValues(lvSuccess).Inc()
		di.metrics.extractDuration.Observe(time.Since(now).Seconds())
	}()

	if err := os.MkdirAll(di.tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	f, err := os.CreateTemp(di.tempDir, buildID)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	// This works because CreateTemp opened a file descriptor and linux keeps a reference count to open
	// files and won't delete them until the ref count is zero.
	defer os.Remove(f.Name())

	span.AddEvent("acquiring reader for objectfile")
	r, release, err := src.Reader()
	if err != nil {
		err = fmt.Errorf("failed to obtain reader for object file: %w", err)

		return nil, err
	}
	span.AddEvent("acquired reader for objectfile")
	defer release()

	if err := di.Extractor.Extract(ctx, f, r); err != nil {
		err = fmt.Errorf("failed to extract debug information: %w", err)
		return nil, err
	}

	release()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to the beginning of the file: %w", err)
	}

	// Try to open the file to make sure it's valid.
	debuginfoFile, err := di.objFilePool.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("failed to open debuginfo file: %w", err)
	}
	return debuginfoFile, nil
}

func (di *Manager) Upload(ctx context.Context, dbg *objectfile.ObjectFile) error {
	defer dbg.HoldOn()

	di.metrics.uploadRequests.Inc()

	ctx, cancel := context.WithTimeout(ctx, di.uploadTimeoutDuration)
	defer cancel()

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.Upload")
	defer span.End()

	buildID := dbg.BuildID
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		defer dbg.HoldOn()

		now := time.Now()
		span.AddEvent("acquiring upload task token")
		// Acquire a token to limit the number of concurrent uploads.
		if err := di.uploadTaskTokens.Acquire(ctx, 1); err != nil {
			err = fmt.Errorf("failed to acquire upload task token: %w", err)
			span.RecordError(err)
			errCh <- err
			return
		}
		di.metrics.uploadInflight.Inc()
		// Observe the time it took to acquire the token.
		di.metrics.uploadRequestWaitDuration.Observe(time.Since(now).Seconds())
		span.AddEvent("acquired upload task token")

		// Release the token when the upload is done.
		defer func() {
			di.uploadTaskTokens.Release(1)
			di.metrics.uploadInflight.Dec()
		}()

		now = time.Now()
		// The singleflight group prevents uploading the same buildID concurrently.
		_, err, shared := di.uploadSingleflight.Do(buildID, func() (interface{}, error) {
			return nil, di.upload(ctx, dbg)
		})
		if shared {
			di.metrics.uploaded.WithLabelValues(lvShared).Inc()
			span.SetAttributes(attribute.Bool("shared", true))
		}
		if err != nil {
			di.uploadSingleflight.Forget(buildID) // Do not cache failed uploads.
			di.metrics.uploaded.WithLabelValues(lvFail).Inc()
			span.RecordError(err)
			errCh <- err
			return
		}
		di.metrics.uploaded.WithLabelValues(lvSuccess).Inc()
		di.metrics.uploadDuration.Observe(time.Since(now).Seconds())
		errCh <- nil
	}()
	return <-errCh
}

func (di *Manager) upload(ctx context.Context, dbg *objectfile.ObjectFile) (err error) { //nolint:nonamedreturns
	defer dbg.HoldOn()

	buildID := dbg.BuildID
	if shouldInitiateUpload := di.shouldInitiate(ctx, buildID, dbg.Path); !shouldInitiateUpload {
		return nil
	}

	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.upload")
	defer span.End()

	defer func() {
		if err != nil {
			span.RecordError(err)
			return
		}
	}()

	di.metrics.uploadAttempts.Inc()

	var (
		// The hash is cached to avoid re-hashing the same binary
		// and getting to the same result again.
		key = hashCacheKey{
			buildID: buildID,
			modtime: dbg.Modtime.Unix(),
		}
		size = dbg.Size
		h    string
	)
	if v, ok := di.hashCache.GetIfPresent(key); ok {
		h = v.(string) //nolint:forcetypeassert
	} else {
		span.AddEvent("acquiring reader for objectfile")
		r, release, err := dbg.Reader()
		if err != nil {
			return fmt.Errorf("failed to obtain reader for object file: %w", err)
		}
		span.AddEvent("acquired reader for objectfile")

		h, err = hash.Reader(r)
		if err != nil {
			release()
			return fmt.Errorf("hash debuginfos: %w", err)
		}
		release()
		di.hashCache.Put(key, h)
	}

	initiateResp, err := di.debuginfoClient.InitiateUpload(ctx, &debuginfopb.InitiateUploadRequest{
		BuildId: buildID,
		Hash:    h,
		Size:    size,
	})
	if err != nil {
		if sts, ok := status.FromError(err); ok {
			if sts.Code() == codes.AlreadyExists {
				di.shouldInitiateCache.Put(buildID, struct{}{})
				return nil
			}
		}
		return fmt.Errorf("initiate upload: %w", err)
	}

	span.AddEvent("acquiring reader for objectfile")
	r, release, err := dbg.Reader()
	if err != nil {
		return fmt.Errorf("failed to obtain reader for object file: %w", err)
	}
	span.AddEvent("acquired reader for objectfile")

	// If we found a debuginfo file, either in file or on the system, we upload it to the server.
	if err := di.uploadFile(ctx, initiateResp.UploadInstructions, r, size); err != nil {
		err = fmt.Errorf("upload debuginfo: %w", err)
		release()
		return err
	}
	release()

	_, err = di.debuginfoClient.MarkUploadFinished(ctx, &debuginfopb.MarkUploadFinishedRequest{
		BuildId:  buildID,
		UploadId: initiateResp.UploadInstructions.UploadId,
	})
	if err != nil {
		return fmt.Errorf("mark upload finished: %w", err)
	}
	return nil
}

func (di *Manager) uploadFile(ctx context.Context, uploadInstructions *debuginfopb.UploadInstructions, r io.Reader, size int64) error {
	switch uploadInstructions.UploadStrategy {
	case debuginfopb.UploadInstructions_UPLOAD_STRATEGY_GRPC:
		return di.uploadViaGRPC(ctx, di.debuginfoClient, uploadInstructions, r)
	case debuginfopb.UploadInstructions_UPLOAD_STRATEGY_SIGNED_URL:
		return di.uploadViaSignedURL(ctx, uploadInstructions.SignedUrl, r, size)
	case debuginfopb.UploadInstructions_UPLOAD_STRATEGY_UNSPECIFIED:
		return fmt.Errorf("upload strategy unspecified, must set one of UPLOAD_STRATEGY_GRPC or UPLOAD_STRATEGY_SIGNED_URL")
	default:
		return fmt.Errorf("unknown upload strategy: %v", uploadInstructions.UploadStrategy)
	}
}

func (di *Manager) uploadViaGRPC(ctx context.Context, debuginfoClient debuginfopb.DebuginfoServiceClient, uploadInstructions *debuginfopb.UploadInstructions, r io.Reader) error {
	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.uploadViaGRPC")
	defer span.End()

	// NewGrpcUploadClient using bufio.NewReader to avoid closing the reader.
	_, err := parcadebuginfo.NewGrpcUploadClient(debuginfoClient).Upload(ctx, uploadInstructions, r)
	return err
}

func (di *Manager) uploadViaSignedURL(ctx context.Context, url string, r io.Reader, size int64) error {
	ctx, span := di.tracer.Start(ctx, "DebuginfoManager.uploadViaSignedURL")
	defer span.End()

	// Uses the default tracer provider and propagator that's set in tracer package,
	// or from the span context passed in.
	ctx = httptrace.WithClientTrace(ctx, otelhttptrace.NewClientTrace(ctx))

	// Client is closing the reader if the reader is also closer.
	// We need to wrap the reader to avoid this.
	// We want to have total control over the reader.
	r = bufio.NewReader(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do upload request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code: %d, msg: %s", resp.StatusCode, string(data))
	}

	return nil
}

func (di *Manager) Close() error {
	var err error
	err = errors.Join(err, di.Finder.Close())
	err = errors.Join(err, di.shouldInitiateCache.Close())
	return err
}

// hasTextSection returns true if the ELF file has a .text section.
func hasTextSection(ef *elf.File) bool {
	if textSection := ef.Section(".text"); textSection == nil {
		return false
	}
	return true
}
