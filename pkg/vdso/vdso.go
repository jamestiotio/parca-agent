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

package vdso

import (
	"errors"
	"fmt"

	"go.uber.org/multierr"

	"github.com/parca-dev/parca/pkg/symbol/symbolsearcher"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/parca-dev/parca-agent/pkg/metadata"
	"github.com/parca-dev/parca-agent/pkg/objectfile"
	"github.com/parca-dev/parca-agent/pkg/process"
)

const (
	lvError   = "error"
	lvSuccess = "success"

	lvErrNotFound        = "not_found"
	lvErrMappingNil      = "mapping_nil"
	lvErrMappingEmpty    = "mapping_empty"
	lvErrAddrOutOfRange  = "addr_out_of_range"
	lvErrBaseCalculation = "base_calculation"
	lvErrUnknown         = "unknown"
)

type metrics struct {
	lookup       *prometheus.CounterVec
	lookupErrors *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		lookup: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "parca_agent_profiler_vdso_lookup_total",
				Help: "Total number of operations of looking up vdso symbols.",
			},
			[]string{"result"},
		),
		lookupErrors: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "parca_agent_profiler_vdso_lookup_errors_total",
				Help: "Total number of errors while looking up vdso symbols.",
			},
			[]string{"type"},
		),
	}
	m.lookup.WithLabelValues(lvSuccess)
	m.lookupErrors.WithLabelValues(lvErrNotFound)
	return m
}

type NoopCache struct{}

func (NoopCache) Resolve(uint64, *process.Mapping) (string, error) { return "", nil }

type Cache struct {
	metrics *metrics

	searcher symbolsearcher.Searcher
	f        string
}

func NewCache(reg prometheus.Registerer, objFilePool *objectfile.Pool) (*Cache, error) {
	kernelVersion, err := metadata.KernelRelease()
	if err != nil {
		return nil, err
	}
	var (
		obj  *objectfile.ObjectFile
		merr error
		path string
	)
	// This file is not present on all systems. It's an optimization.
	for _, vdso := range []string{"vdso.so", "vdso64.so"} {
		path = fmt.Sprintf("/usr/lib/modules/%s/vdso/%s", kernelVersion, vdso)
		obj, err = objFilePool.Open(path)
		if err != nil {
			merr = multierr.Append(merr, fmt.Errorf("failed to open elf file: %s, err: %w", path, err))
			continue
		}
		defer obj.HoldOn()
		break
	}
	if obj == nil {
		return nil, merr
	}

	ef, release, err := obj.ELF()
	if err != nil {
		return nil, fmt.Errorf("failed to get elf file: %s, err: %w", path, err)
	}
	defer release()

	// output of readelf --dyn-syms vdso.so:
	//  Num:    Value          Size Type    Bind   Vis      Ndx Name
	//     0: 0000000000000000     0 NOTYPE  LOCAL  DEFAULT  UND
	//     1: ffffffffff700354     0 SECTION LOCAL  DEFAULT    7
	//     2: ffffffffff700700  1389 FUNC    WEAK   DEFAULT   13 clock_gettime@@LINUX_2.6
	//     3: 0000000000000000     0 OBJECT  GLOBAL DEFAULT  ABS LINUX_2.6
	//     4: ffffffffff700c70   734 FUNC    GLOBAL DEFAULT   13 __vdso_gettimeofday@@LINUX_2.6
	//     5: ffffffffff700f70    61 FUNC    GLOBAL DEFAULT   13 __vdso_getcpu@@LINUX_2.6
	//     6: ffffffffff700c70   734 FUNC    WEAK   DEFAULT   13 gettimeofday@@LINUX_2.6
	//     7: ffffffffff700f50    22 FUNC    WEAK   DEFAULT   13 time@@LINUX_2.6
	//     8: ffffffffff700f70    61 FUNC    WEAK   DEFAULT   13 getcpu@@LINUX_2.6
	//     9: ffffffffff700700  1389 FUNC    GLOBAL DEFAULT   13 __vdso_clock_gettime@@LINUX_2.6
	//    10: ffffffffff700f50    22 FUNC    GLOBAL DEFAULT   13 __vdso_time@@LINUX_2.6
	syms, err := ef.DynamicSymbols()
	if err != nil {
		return nil, err
	}
	return &Cache{newMetrics(reg), symbolsearcher.New(syms), path}, nil
}

func (c *Cache) Resolve(addr uint64, m *process.Mapping) (string, error) {
	if c == nil {
		return "", nil
	}
	if m == nil {
		c.metrics.lookupErrors.WithLabelValues(lvError).Inc()
		return "", errors.New("mapping is nil")
	}
	addr, err := m.Normalize(addr)
	if err != nil {
		c.metrics.lookupErrors.WithLabelValues(lvError).Inc()
		var addrErr *process.AddressOutOfRangeError
		switch {
		case errors.As(err, &addrErr):
			c.metrics.lookupErrors.WithLabelValues(lvErrAddrOutOfRange).Inc()
		case errors.Is(err, process.ErrBaseAddressCannotCalculated):
			c.metrics.lookupErrors.WithLabelValues(lvErrBaseCalculation).Inc()
		default:
			c.metrics.lookupErrors.WithLabelValues(lvErrUnknown).Inc()
		}
		return "", err
	}

	sym, err := c.searcher.Search(addr)
	if err != nil {
		c.metrics.lookupErrors.WithLabelValues(lvError).Inc()
		c.metrics.lookupErrors.WithLabelValues(lvErrNotFound).Inc()
		return "", err
	}
	c.metrics.lookup.WithLabelValues(lvSuccess).Inc()
	return sym, nil
}
