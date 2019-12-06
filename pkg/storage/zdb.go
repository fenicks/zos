package storage

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/google/uuid"

	"github.com/pkg/errors"

	"github.com/threefoldtech/zos/pkg"
	"github.com/threefoldtech/zos/pkg/storage/filesystem"
)

// Allocation describes a zdb insance that is good for namespace allocation
type Allocation struct {
	VolumeID   string
	VolumePath string
}

type zdbNamespace struct {
	ID   string      `json:"-"`
	Size uint64      `json:"size"`
	Mode pkg.ZDBMode `json:"mode"`
}

type zdbNamespaces []zdbNamespace

func (s zdbNamespaces) Total() (total uint64) {
	for i := range s {
		total += s[i].Size
	}

	return
}

func isZdbVolume(volume filesystem.Volume) bool {
	return strings.HasPrefix(volume.Name(), zdbPoolPrefix)
}

type zdbVolume struct {
	filesystem.Volume
}

func (v *zdbVolume) CreateNamespace(ns string, info zdbNamespace) error {
	dir := filepath.Join(v.Path(), ns)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	infoFile, err := os.Create(filepath.Join(dir, ".info"))
	if err != nil {
		return err
	}
	defer infoFile.Close()

	enc := json.NewEncoder(infoFile)
	return enc.Encode(info)
}

func (v *zdbVolume) GetNamespace(ns string) (info zdbNamespace, err error) {
	infoPath := filepath.Join(v.Path(), ns, ".info")

	infoFile, err := os.Open(infoPath)
	if err != nil {
		return info, err
	}

	defer infoFile.Close()

	dec := json.NewDecoder(infoFile)
	err = dec.Decode(&info)
	info.ID = ns
	return info, err
}

func (v *zdbVolume) ListNamespaces() (zdbNamespaces, error) {
	dirs, err := ioutil.ReadDir(v.Path())
	if err != nil {
		return nil, err
	}

	var namespaces zdbNamespaces
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}

		info, err := v.GetNamespace(dir.Name())
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		namespaces = append(namespaces, info)
	}

	return namespaces, nil
}

func (s *storageModule) Allocate2(nsID string, diskType pkg.DeviceType, size uint64, mode pkg.ZDBMode) (allocation Allocation, err error) {
	log := log.With().
		Str("type", string(diskType)).
		Uint64("size", size).
		// Str("mode", string(mode)). TODO: currently the mode is not used
		Logger()

	log.Info().Msg("try to allocation space for 0-DB")

	for _, pool := range s.volumes {

		// skip pool with wrong disk type
		if pool.Type() != diskType {
			continue
		}

		volumes, err := pool.Volumes()
		if err != nil {
			return allocation, errors.Wrapf(err, "failed to list volume on pool %s", pool.Name())
		}

		for _, volume := range volumes {
			// skip all non-zdb volume
			if !isZdbVolume(volume) {
				continue
			}

			zdb := zdbVolume{volume}

			_, err := zdb.GetNamespace(nsID)
			if os.IsNotExist(err) {
				continue
			} else if err != nil {
				return allocation, errors.Wrapf(err, "failed to get namespace info on volume '%s'", volume.Path())
			}

			// we found the namespace
			allocation = Allocation{
				VolumeID:   zdb.Name(),
				VolumePath: zdb.Path(),
			}

			return allocation, nil
		}
	}

	type Candidate struct {
		filesystem.Volume
		Free uint64
	}

	var candidates []Candidate
	// okay, so this is a new allocation
	for _, pool := range s.volumes {
		// skip pool with wrong disk type
		if pool.Type() != diskType {
			continue
		}

		usage, err := pool.Usage()
		if err != nil {
			return allocation, errors.Wrapf(err, "failed to read usage of pool %s", pool.Name())
		}

		volumes, err := pool.Volumes()
		if err != nil {
			return allocation, errors.Wrapf(err, "failed to list volume on pool %s", pool.Name())
		}

		for _, volume := range volumes {
			// skip all non-zdb volume
			if !isZdbVolume(volume) {
				continue
			}

			zdb := zdbVolume{volume}

			namespaces, err := zdb.ListNamespaces()
			if err != nil {
				return allocation, errors.Wrapf(err, "failed to list namespaces from volume '%s'", volume.Path())
			}

			quota := namespaces.Total()
			if quota+size > usage.Size {
				// not enough space for this namespace
				continue
			}

			candidates = append(
				candidates,
				Candidate{
					Volume: volume,
					Free:   usage.Size - (quota + size),
				})

			return allocation, nil
		}
	}

	var volume filesystem.Volume
	if len(candidates) > 0 {
		// reverse sort by free space
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Free > candidates[j].Free
		})

		volume = candidates[0]
	} else {
		// no candidates, so we have to try to create a new subvolume.
		name, err := genZDBPoolName()
		if err != nil {
			return allocation, errors.Wrap(err, "failed to generate new sub-volume name")
		}

		//TODO: this createSubvol is not good for the zdb use case since it requires a
		// NON zero size. while we need to create a zdb with an unlimited size.
		volume, err = s.createSubvol(size, name, diskType)
		if err != nil {
			return allocation, errors.Wrap(err, "failed to create sub-volume")
		}

	}

	zdb := zdbVolume{volume}

	if err := zdb.CreateNamespace(nsID, zdbNamespace{
		Size: size,
		Mode: mode,
	}); err != nil {
		return allocation, errors.Wrapf(err, "failed to create namespace directory: '%s/%s'", zdb.Path(), nsID)
	}

	return Allocation{
		VolumeID:   volume.Name(),
		VolumePath: volume.Path(),
	}, nil

}

// Allocate is responsible to make sure the subvolume used by a 0-db as enough storage capacity
// of specified size, type and mode
// it returns the volume ID and its path or an error if it couldn't allocate enough storage
func (s *storageModule) Allocate(diskType pkg.DeviceType, size uint64, mode pkg.ZDBMode) (string, string, error) {
	// try to find an existing zdb volume that has still enough storage available
	// if we find it, grow the quota by the requested size
	// if we don't, pick a new pool and create a zdb volume on it with the requested size
	slog := log.With().
		Str("type", string(diskType)).
		Uint64("size", size).
		// Str("mode", string(mode)). TODO: currently the mode is not used
		Logger()

	slog.Info().Msg("try to allocation space for 0-DB")

	for _, pool := range s.volumes {

		// skip pool with wrong disk type
		if pool.Type() != diskType {
			continue
		}

		usage, err := pool.Usage()
		if err != nil {
			return "", "", errors.Wrapf(err, "failed to read usage of pool %s", pool.Name())
		}

		reserved, err := pool.Reserved()
		if err != nil {
			return "", "", errors.Wrapf(err, "failed to read reserved size of pool %s", pool.Name())
		}

		// Make sure adding this filesystem would not bring us over the disk limit
		if reserved+size > usage.Size {
			slog.Info().Msgf("Disk does not have enough space left to hold filesystem")
			continue
		}

		volumes, err := pool.Volumes()
		if err != nil {
			return "", "", errors.Wrapf(err, "failed to list volume on pool %s", pool.Name())
		}

		for _, volume := range volumes {

			// skip all non-zdb volume
			if !strings.HasPrefix(volume.Name(), zdbPoolPrefix) {
				continue
			}

			usage, err := volume.Usage()
			if err != nil {
				return "", "", errors.Wrapf(err, "failed to read usage of volume %s", volume.Name())
			}

			// existing volume with enough storage, grow its limit
			if err := volume.Limit(usage.Size + size); err != nil {
				return "", "", errors.Wrapf(err, "failed to grow limit of volume %s", volume.Name())
			}

			slog.Info().
				Str("volume", volume.Name()).
				Str("path", volume.Path()).
				Msg("space allocated")
			return volume.Name(), volume.Path(), nil
		}
	}

	name, err := genZDBPoolName()
	if err != nil {
		return "", "", errors.Wrap(err, "failed to generate new sub-volume name")
	}

	volume, err := s.createSubvol(size, name, diskType)
	if err != nil {
		return "", "", errors.Wrap(err, "failed to create sub-volume")
	}

	slog.Info().
		Str("volume", volume.Name()).
		Str("path", volume.Path()).
		Msg("space allocated")
	return volume.Name(), volume.Path(), nil
}

// Claim let the system claim the allocated storage used by a 0-db namespace
func (s *storageModule) Claim(name string, size uint64) error {
	var (
		v   filesystem.Volume
		err error
	)

	for _, pool := range s.volumes {

		volumes, err := pool.Volumes()
		if err != nil {
			return errors.Wrapf(err, "failed to list volume on pool %s", pool.Name())
		}

		for _, volume := range volumes {
			if volume.Name() == name {
				v = volume
				break
			}
		}

		if v != nil {
			break
		}
	}

	if v == nil {
		return fmt.Errorf("volume named %s not found", name)
	}

	usage, err := v.Usage()
	if err != nil {
		return err
	}

	// limit cannot be 0 cause 0 means not limited
	limit := usage.Size - size
	if limit <= 0 {
		limit = 1
	}

	// shrink the limit
	if err := v.Limit(usage.Size - size); err != nil {
		return errors.Wrapf(err, "failed to grow limit of volume %s", v.Name())
	}

	return nil
}

const zdbPoolPrefix = "zdb"

func genZDBPoolName() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	name := zdbPoolPrefix + id.String()
	return name, nil
}
