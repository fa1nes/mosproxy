package router

import (
	"bytes"
	"fmt"
	"net/netip"

	"github.com/IrineSistiana/mosproxy/internal/ipmarker"
	"github.com/rs/zerolog"
)

func (r *Router) loadEcsZone(fps []string) error {
	parseFn := func(b []byte) (*ipmarker.IpMarker, error) {
		return ipmarker.LoadIpMarkerFromReader(bytes.NewReader(b))
	}
	vInfo := func(e *zerolog.Event, v *ipmarker.IpMarker) {
		e.Int("ip_len", v.IpLen()).Int("zone_len", v.MarkLen())
	}
	s := make([]*fileLoader[ipmarker.IpMarker], 0)
	for _, fp := range fps {
		logger := r.logger.With().Str("ecs_zone", fp).Logger()
		loader := &fileLoader[ipmarker.IpMarker]{
			fp:      fp,
			parseFn: parseFn,
			logger:  &logger,
			vInfo:   vInfo,
		}
		_, err := loader.init()
		if err != nil {
			return fmt.Errorf("failed to load ecs zone from file %s, %w", fp, err)
		}
		s = append(s, loader)
	}
	r.ecsZone = &ECSZone{fileLoaderGroup: s}
	return nil
}

type ECSZone struct {
	fileLoaderGroup[ipmarker.IpMarker]
}

func (z *ECSZone) Mark(addr netip.Addr) (string, bool) {
	for _, loader := range z.fileLoaderGroup {
		s, ok := loader.V().Mark(addr)
		if ok {
			return s, true
		}
	}
	return "", false
}

type ECSZoneOverWrite struct {
	fileLoaderGroup[map[string]netip.Prefix]
}

func (g *ECSZoneOverWrite) Get(z string) (netip.Prefix, bool) {
	for _, loader := range g.fileLoaderGroup {
		m := loader.V()
		if m != nil {
			p, ok := (*m)[z]
			if ok {
				return p, true
			}
		}
	}
	return netip.Prefix{}, false
}

// Each calls fn for every zone in every loaded file. A zone listed in more
// than one file is reported once, from the first file, like Get.
func (g *ECSZoneOverWrite) Each(fn func(zone string, p netip.Prefix)) {
	seen := make(map[string]struct{})
	for _, loader := range g.fileLoaderGroup {
		m := loader.V()
		if m == nil {
			continue
		}
		for z, p := range *m {
			if _, dup := seen[z]; dup {
				continue
			}
			seen[z] = struct{}{}
			fn(z, p)
		}
	}
}

func (r *Router) loadEcsZoneOverwrite(fps []string) error {
	g, err := r.loadMark2Prefix(fps, "ecs_zone_overwrite")
	if err != nil {
		return err
	}
	r.ecsZoneOverwrite = g
	return nil
}

func (r *Router) loadMark2Prefix(fps []string, name string) (*ECSZoneOverWrite, error) {
	parseFn := func(b []byte) (*map[string]netip.Prefix, error) {
		m, err := ipmarker.LoadMark2PrefixFromReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		return &m, nil
	}
	vInfo := func(e *zerolog.Event, v *map[string]netip.Prefix) {
		e.Int("len", len(*v))
	}
	s := make([]*fileLoader[map[string]netip.Prefix], 0)
	for _, fp := range fps {
		logger := r.logger.With().Str(name, fp).Logger()
		loader := &fileLoader[map[string]netip.Prefix]{
			fp:      fp,
			parseFn: parseFn,
			logger:  &logger,
			vInfo:   vInfo,
		}
		_, err := loader.init()
		if err != nil {
			return nil, fmt.Errorf("failed to load %s from file %s, %w", name, fp, err)
		}
		s = append(s, loader)
	}
	return &ECSZoneOverWrite{fileLoaderGroup: s}, nil
}
