package components

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/src-d/engine/docker"
)

// cli version set by src-d command
var cliVersion = ""

// SetCliVersion sets cli version
func SetCliVersion(v string) {
	cliVersion = v
	Daemon.Version = v
}

var srcdNamespaces = []string{
	"srcd",
	"bblfsh",
}

type Component struct {
	Name    string
	Image   string
	Version string // only if there's a required version

	retrieveVersionFunc func(*Component) (string, bool, error)
}

func (c *Component) ImageWithVersion() string {
	return fmt.Sprintf("%s:%s", c.Image, c.Version)
}

// Kill removes the Component container. If it is not running it returns nil
func (c *Component) Kill() error {
	err := docker.RemoveContainer(c.Name)
	if err != nil && err != docker.ErrNotFound {
		return err
	}

	return nil
}

// IsInstalled returns true if the Component image is installed with the
// exact version
func (c *Component) IsInstalled(ctx context.Context) (bool, error) {
	return IsInstalled(ctx, c.ImageWithVersion())
}

// IsRunning returns true if the Component container is running using the
// exact image version
func (c *Component) IsRunning() (bool, error) {
	return docker.IsRunning(c.Name, c.ImageWithVersion())
}

// RetrieveVersion updates the Version field with a compatible tag for the
// image based on the current fixed version; it returns true if there are any
// newer versions with breaking changes
func (c *Component) RetrieveVersion() (bool, error) {
	if c.retrieveVersionFunc == nil {
		return false, nil
	}

	v, hasNew, err := c.retrieveVersionFunc(c)

	if err == nil {
		c.Version = v
	}

	return hasNew, err
}

func daemonRetrieveVersion(daemon *Component) (string, bool, error) {
	return docker.GetCompatibleTag(daemon.Image, cliVersion)
}

const (
	BblfshVolume = "srcd-cli-bblfsh-storage"
)

var (
	Gitbase = Component{
		Name:    "srcd-cli-gitbase",
		Image:   "srcd/gitbase",
		Version: "v0.17.1",
	}

	GitbaseWeb = Component{
		Name:    "srcd-cli-gitbase-web",
		Image:   "srcd/gitbase-web",
		Version: "v0.3.1",
	}

	Bblfshd = Component{
		Name:    "srcd-cli-bblfshd",
		Image:   "bblfsh/bblfshd",
		Version: "v2.9.2-drivers",
	}

	BblfshWeb = Component{
		Name:    "srcd-cli-bblfsh-web",
		Image:   "bblfsh/web",
		Version: "v0.8.1",
	}

	Daemon = Component{
		Name:  "srcd-cli-daemon",
		Image: "srcd/cli-daemon",
		// Version
		retrieveVersionFunc: daemonRetrieveVersion,
	}

	workDirDependants = []Component{
		Daemon,
		Gitbase,
		Bblfshd, // does not depend on workdir but it does depend on user dir
	}
)

// FilterFunc is a filtering function for List.
type FilterFunc func(Component) (bool, error)

func filter(cmps []Component, filters []FilterFunc) ([]Component, error) {
	var result []Component
	for _, cmp := range cmps {
		var add = true
		for _, f := range filters {
			ok, err := f(cmp)
			if err != nil {
				return nil, err
			}

			if !ok {
				add = false
				break
			}
		}

		if add {
			result = append(result, cmp)
		}
	}
	return result, nil
}

// IsWorkingDirDependant filters Components that depend on the working directory.
var IsWorkingDirDependant FilterFunc = func(cmp Component) (bool, error) {
	for _, c := range workDirDependants {
		if c.Image == cmp.Image {
			return true, nil
		}
	}
	return false, nil
}

// IsInstalledFilter filters Components that have its image installed, with
// the exact version
var IsInstalledFilter FilterFunc = func(cmp Component) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return docker.IsInstalled(ctx, cmp.Image, cmp.Version)
}

// IsRunningFilter filters Components that have a container running, using
// its image with the exact version
var IsRunningFilter FilterFunc = func(cmp Component) (bool, error) {
	r, err := cmp.IsRunning()
	if err != nil {
		return false, nil
	}

	return r, nil
}

// List returns the list of known Components, which may or may not be installed.
// If allVersions is true other Components with image versions different from
// the current ones will be included.
func List(ctx context.Context, allVersions bool, filters ...FilterFunc) ([]Component, error) {
	componentsList := []Component{
		Daemon,
		Gitbase,
		GitbaseWeb,
		Bblfshd,
		BblfshWeb,
	}

	if allVersions {
		otherComponents := make([]Component, 0)

		for _, cmp := range componentsList {
			newCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			// Look for any other image version that might be installed
			versions, err := docker.VersionsInstalled(newCtx, cmp.Image)
			if err != nil {
				return nil, err
			}

			for _, v := range versions {
				if v == cmp.Version {
					// Already added before
					continue
				}

				otherComponents = append(otherComponents, Component{
					Name:    cmp.Name,
					Image:   cmp.Image,
					Version: v,
				})
			}
		}

		componentsList = append(componentsList, otherComponents...)
	}

	if len(filters) > 0 {
		return filter(componentsList, filters)
	}

	return componentsList, nil
}

var ErrNotSrcd = fmt.Errorf("not srcd component")

// Install installs a new component.
func Install(ctx context.Context, id string) error {
	if !isSrcdComponent(id) {
		return ErrNotSrcd
	}

	image, version := docker.SplitImageID(id)
	return docker.Pull(ctx, image, version)
}

func IsInstalled(ctx context.Context, id string) (bool, error) {
	if !isSrcdComponent(id) {
		return false, ErrNotSrcd
	}

	image, version := docker.SplitImageID(id)
	return docker.IsInstalled(ctx, image, version)
}

func Stop() error {
	logrus.Info("stopping containers...")

	// we actually not just stop but remove containers here
	// it's needed to make sure configuration of the containers is correct
	// without over-complicated logic for it
	if err := removeContainers(); err != nil {
		return errors.Wrap(err, "unable to stop all containers")
	}

	return nil
}

func Prune(images bool) error {
	logrus.Info("removing containers...")
	if err := removeContainers(); err != nil {
		return errors.Wrap(err, "unable to remove all containers")
	}

	logrus.Info("removing volumes...")

	if err := removeVolumes(); err != nil {
		return errors.Wrap(err, "unable to remove volumes")
	}

	logrus.Info("removing network...")

	if err := docker.RemoveNetwork(context.Background()); err != nil {
		return errors.Wrap(err, "unable to remove network")
	}

	if images {
		logrus.Info("removing images...")

		if err := removeImages(); err != nil {
			return errors.Wrap(err, "unable to remove all images")
		}
	}

	return nil
}

func removeContainers() error {
	cs, err := docker.List()
	if err != nil {
		return err
	}

	for _, c := range cs {
		if len(c.Names) == 0 {
			continue
		}

		name := strings.TrimLeft(c.Names[0], "/")
		if isFromEngine(name) {
			logrus.Infof("removing container %s", name)

			if err := docker.RemoveContainer(name); err != nil {
				return err
			}
		}
	}

	return nil
}

func removeVolumes() error {
	vols, err := docker.ListVolumes(context.Background())
	if err != nil {
		return err
	}

	for _, vol := range vols {
		if isFromEngine(vol.Name) {
			logrus.Infof("removing volume %s", vol.Name)

			if err := docker.RemoveVolume(context.Background(), vol.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

func removeImages() error {
	cmps, err := List(context.Background(), true, IsInstalledFilter)
	if err != nil {
		return errors.Wrap(err, "unable to list images")
	}

	for _, cmp := range cmps {
		logrus.Infof("removing image %s", cmp.ImageWithVersion())

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := docker.RemoveImage(ctx, cmp.ImageWithVersion()); err != nil {
			return err
		}
	}

	return nil
}

func stringInSlice(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

// isSrcdComponent returns true if the Image repository (id) belongs to src-d
func isSrcdComponent(id string) bool {
	namespace := strings.Split(id, "/")[0]
	return stringInSlice(srcdNamespaces, namespace)
}

func isFromEngine(name string) bool {
	return strings.HasPrefix(name, "srcd-cli-")
}
