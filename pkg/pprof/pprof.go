// Copyright 2023 The Parca Authors
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

package pprof

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	pprofprofile "github.com/google/pprof/profile"

	"github.com/parca-dev/parca-agent/pkg/ksym"
	"github.com/parca-dev/parca-agent/pkg/perf"
	"github.com/parca-dev/parca-agent/pkg/process"
	"github.com/parca-dev/parca-agent/pkg/profile"
	"github.com/parca-dev/parca-agent/pkg/profiler"
)

type VDSOSymbolizer interface {
	Resolve(addr uint64, m *process.Mapping) (string, error)
}

type Converter struct {
	logger log.Logger

	addressNormalizer       profiler.AddressNormalizer
	ksym                    *ksym.Ksym
	vdsoSymbolizer          VDSOSymbolizer
	metrics                 *ConverterMetrics
	perfMapCache            *perf.PerfMapCache
	jitdumpCache            *perf.JitdumpCache
	disableJITSymbolization bool

	// We already have the perf map cache but it Stats() the perf map on every
	// cache retrieval, but we only want to do that once per conversion.
	cachedPerfMap    *perf.Map
	cachedPerfMapErr error

	cachedJitdump    map[string]*perf.Map
	cachedJitdumpErr map[string]error

	functionIndex        map[string]*pprofprofile.Function
	addrLocationIndex    map[uint64]*pprofprofile.Location
	perfmapLocationIndex map[string]*pprofprofile.Location
	jitdumpLocationIndex map[string]*pprofprofile.Location
	kernelLocationIndex  map[string]*pprofprofile.Location
	vdsoLocationIndex    map[string]*pprofprofile.Location

	pid           int
	mappings      []*process.Mapping
	kernelMapping *pprofprofile.Mapping

	result *pprofprofile.Profile
}

func NewConverter(
	logger log.Logger,
	addressNormalizer profiler.AddressNormalizer,
	ksym *ksym.Ksym,
	vdsoSymbolizer VDSOSymbolizer,
	perfMapCache *perf.PerfMapCache,
	jitdumpCache *perf.JitdumpCache,
	metrics *ConverterMetrics,
	disableJITSymbolization bool,

	pid int,
	mappings process.Mappings,
	captureTime time.Time,
	periodNS int64,
) *Converter {
	pprofMappings := mappings.ConvertToPprof()
	kernelMapping := &pprofprofile.Mapping{
		ID:   uint64(len(pprofMappings)) + 1, // +1 because pprof uses 1-indexing to be able to differentiate from 0 (unset).
		File: "[kernel.kallsyms]",
	}
	pprofMappings = append(pprofMappings, kernelMapping)

	return &Converter{
		logger:                  log.With(logger, "pid", pid),
		addressNormalizer:       addressNormalizer,
		ksym:                    ksym,
		vdsoSymbolizer:          vdsoSymbolizer,
		perfMapCache:            perfMapCache,
		jitdumpCache:            jitdumpCache,
		metrics:                 metrics,
		disableJITSymbolization: disableJITSymbolization,

		cachedJitdump:    map[string]*perf.Map{},
		cachedJitdumpErr: map[string]error{},

		functionIndex:        map[string]*pprofprofile.Function{},
		addrLocationIndex:    map[uint64]*pprofprofile.Location{},
		perfmapLocationIndex: map[string]*pprofprofile.Location{},
		jitdumpLocationIndex: map[string]*pprofprofile.Location{},
		kernelLocationIndex:  map[string]*pprofprofile.Location{},
		vdsoLocationIndex:    map[string]*pprofprofile.Location{},

		pid:           pid,
		mappings:      mappings,
		kernelMapping: kernelMapping,

		result: &pprofprofile.Profile{
			TimeNanos:     captureTime.UnixNano(),
			DurationNanos: int64(time.Since(captureTime)),
			Period:        periodNS,
			SampleType: []*pprofprofile.ValueType{{
				Type: "samples",
				Unit: "count",
			}},
			// Sampling at 100Hz would be every 10 Million nanoseconds.
			PeriodType: &pprofprofile.ValueType{
				Type: "cpu",
				Unit: "nanoseconds",
			},
			Mapping: pprofMappings,
		},
	}
}

// Convert converts a profile to a pprof profile. It is intended to only be
// used once.
func (c *Converter) Convert(ctx context.Context, rawData []profile.RawSample) (*pprofprofile.Profile, error) {
	kernelAddresses := map[uint64]struct{}{}
	for _, sample := range rawData {
		for _, addr := range sample.KernelStack {
			kernelAddresses[addr] = struct{}{}
		}
	}

	kernelSymbols, err := c.ksym.Resolve(kernelAddresses)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to resolve kernel symbols skipping profile", "err", err)
		kernelSymbols = map[uint64]string{}
	}

	for _, sample := range rawData {
		pprofSample := &pprofprofile.Sample{
			Value:    []int64{int64(sample.Value)},
			Location: make([]*pprofprofile.Location, 0, len(sample.UserStack)+len(sample.KernelStack)),
		}

		for _, addr := range sample.KernelStack {
			l := c.addKernelLocation(c.kernelMapping, kernelSymbols, addr)
			pprofSample.Location = append(pprofSample.Location, l)
		}

		for _, addr := range sample.UserStack {
			mappingIndex := mappingForAddr(c.result.Mapping, addr)
			if mappingIndex == -1 {
				c.metrics.frameDrop.WithLabelValues(labelFrameDropReasonMappingNil).Inc()
				// Normalization will fail anyway, so we can skip this frame.
				continue
			}

			processMapping := c.mappings[mappingIndex]
			pprofMapping := c.result.Mapping[mappingIndex]
			switch {
			case pprofMapping.File == "[vdso]":
				pprofSample.Location = append(pprofSample.Location, c.addVDSOLocation(processMapping, pprofMapping, addr))
			case pprofMapping.File == "jit":
				pprofSample.Location = append(pprofSample.Location, c.addPerfMapLocation(pprofMapping, addr))
			case strings.HasSuffix(pprofMapping.File, ".dump"):
				// TODO: The .dump is only a convention, it doesn't have to
				// have this suffix. Better would be to check the magic number
				// of the mapping file:
				// https://elixir.bootlin.com/linux/v4.10/source/tools/perf/Documentation/jitdump-specification.txt
				pprofSample.Location = append(pprofSample.Location, c.addJITDumpLocation(pprofMapping, addr, pprofMapping.File))
			default:
				pprofSample.Location = append(pprofSample.Location, c.addAddrLocation(processMapping, pprofMapping, addr))
			}
		}

		c.result.Sample = append(c.result.Sample, pprofSample)
	}

	return c.result, nil
}

func mappingForAddr(mappings []*pprofprofile.Mapping, addr uint64) int {
	for i, m := range mappings {
		if m.Start <= addr && addr < m.Limit {
			return i
		}
	}
	return -1
}

func (c *Converter) addKernelLocation(
	m *pprofprofile.Mapping,
	kernelSymbols map[uint64]string,
	addr uint64,
) *pprofprofile.Location {
	kernelSymbol, ok := kernelSymbols[addr]
	if !ok {
		kernelSymbol = "not found"
	}

	if l, ok := c.kernelLocationIndex[kernelSymbol]; ok {
		return l
	}

	l := &pprofprofile.Location{
		ID:      uint64(len(c.result.Location)) + 1,
		Mapping: m,
		Line: []pprofprofile.Line{{
			Function: c.addFunction(kernelSymbol),
		}},
	}

	c.kernelLocationIndex[kernelSymbol] = l
	c.result.Location = append(c.result.Location, l)

	return l
}

func (c *Converter) addVDSOLocation(
	processMapping *process.Mapping,
	m *pprofprofile.Mapping,
	addr uint64,
) *pprofprofile.Location {
	functionName, err := c.vdsoSymbolizer.Resolve(addr, processMapping)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to symbolize VDSO address", "address", fmt.Sprintf("%x", addr), "err", err)
		functionName = "unknown"
	}

	if l, ok := c.vdsoLocationIndex[functionName]; ok {
		return l
	}

	l := &pprofprofile.Location{
		ID:      uint64(len(c.result.Location)) + 1,
		Mapping: m,
		Line: []pprofprofile.Line{{
			Function: c.addFunction(functionName),
		}},
	}

	c.vdsoLocationIndex[functionName] = l
	c.result.Location = append(c.result.Location, l)

	return l
}

func (c *Converter) addAddrLocation(
	processMapping *process.Mapping,
	m *pprofprofile.Mapping,
	addr uint64,
) *pprofprofile.Location {
	normalizedAddress, err := c.addressNormalizer.Normalize(processMapping, addr)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to normalize address", "address", fmt.Sprintf("%x", addr), "err", err)
		normalizedAddress = addr
	}

	return c.addAddrLocationNoNormalization(m, normalizedAddress)
}

func (c *Converter) addAddrLocationNoNormalization(m *pprofprofile.Mapping, addr uint64) *pprofprofile.Location {
	if l, ok := c.addrLocationIndex[addr]; ok {
		return l
	}

	l := &pprofprofile.Location{
		ID:      uint64(len(c.result.Location)) + 1,
		Mapping: m,
		Address: addr,
	}

	c.addrLocationIndex[addr] = l
	c.result.Location = append(c.result.Location, l)

	return l
}

func (c *Converter) addPerfMapLocation(
	m *pprofprofile.Mapping,
	addr uint64,
) *pprofprofile.Location {
	if c.disableJITSymbolization {
		return c.addAddrLocationNoNormalization(m, addr)
	}

	perfMap, err := c.perfMap()
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to get perf map for PID", "err", err)
	}

	if perfMap == nil {
		return c.addAddrLocationNoNormalization(m, addr)
	}

	symbol, err := perfMap.Lookup(addr)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to lookup symbol for address", "address", fmt.Sprintf("%x", addr), "err", err)
		return c.addAddrLocationNoNormalization(m, addr)
	}

	if l, ok := c.perfmapLocationIndex[symbol]; ok {
		return l
	}

	l := &pprofprofile.Location{
		ID:      uint64(len(c.result.Location)) + 1,
		Mapping: m,
		Line: []pprofprofile.Line{{
			Function: c.addFunction(symbol),
		}},
	}

	c.perfmapLocationIndex[symbol] = l
	c.result.Location = append(c.result.Location, l)
	return l
}

func (c *Converter) perfMap() (*perf.Map, error) {
	if c.cachedPerfMap != nil || c.cachedPerfMapErr != nil {
		return c.cachedPerfMap, c.cachedPerfMapErr
	}

	c.cachedPerfMap, c.cachedPerfMapErr = c.perfMapCache.PerfMapForPID(c.pid)
	return c.cachedPerfMap, c.cachedPerfMapErr
}

func (c *Converter) addJITDumpLocation(
	m *pprofprofile.Mapping,
	addr uint64,
	path string,
) *pprofprofile.Location {
	if c.disableJITSymbolization {
		return c.addAddrLocationNoNormalization(m, addr)
	}

	jitdump, err := c.jitdump(path)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to get perf map for PID", "err", err)
	}

	if jitdump == nil {
		return c.addAddrLocationNoNormalization(m, addr)
	}

	symbol, err := jitdump.Lookup(addr)
	if err != nil {
		level.Debug(c.logger).Log("msg", "failed to lookup symbol for address", "address", fmt.Sprintf("%x", addr), "err", err)
		return c.addAddrLocationNoNormalization(m, addr)
	}

	if l, ok := c.jitdumpLocationIndex[symbol]; ok {
		return l
	}

	l := &pprofprofile.Location{
		ID:      uint64(len(c.result.Location)) + 1,
		Mapping: m,
		Line: []pprofprofile.Line{{
			Function: c.addFunction(symbol),
		}},
	}

	c.jitdumpLocationIndex[symbol] = l
	c.result.Location = append(c.result.Location, l)
	return l
}

func (c *Converter) jitdump(path string) (*perf.Map, error) {
	jitdump, jitdumpExists := c.cachedJitdump[path]
	jitdumpErr, jitdumpErrExists := c.cachedJitdumpErr[path]
	if jitdumpExists || jitdumpErrExists {
		return jitdump, jitdumpErr
	}

	jitdump, err := c.jitdumpCache.JitdumpForPID(c.pid, path)
	c.cachedJitdump[path] = jitdump
	c.cachedJitdumpErr[path] = err
	return jitdump, err
}

// TODO: add support for filename and startLine of functions.
func (c *Converter) addFunction(
	name string,
) *pprofprofile.Function {
	if f, ok := c.functionIndex[name]; ok {
		return f
	}

	f := &pprofprofile.Function{
		ID:   uint64(len(c.result.Function) + 1),
		Name: name,
	}

	c.functionIndex[name] = f
	c.result.Function = append(c.result.Function, f)

	return f
}
