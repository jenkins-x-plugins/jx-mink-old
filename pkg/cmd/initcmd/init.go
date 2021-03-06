package initcmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/jenkins-x-plugins/jx-mink/pkg/rootcmd"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/go-git/go-git/v5"
)

const (
	minkFileName = ".mink.yaml"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Creates a .mink.yaml file if one does not exist and the project has a Dockerfile or buildpack overrides.toml and adds a link to the mink image in the charts values.yaml file
`)

	cmdExample = templates.Examples(`
		# ensures there is a .mink.yaml file for the project if there is a Dockerfile 
		# and references it in the charts values.yaml
		%s mink
	`)
)

// Options the options for the command
type Options struct {
	options.BaseOptions
	Dir           string
	Dockerfile    string
	MinkEnabled   bool
	NoGit         bool
	CommandRunner cmdrunner.CommandRunner
}

// NewCmdMinkInit creates a command object for the command
func NewCmdMinkInit() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use: "init",

		Short:   "Creates a .mink.yaml file if one does not exist and the project has a Dockerfileor a buildpack overrides.toml",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the directory to use")
	cmd.Flags().StringVarP(&o.Dockerfile, "dockerfile", "f", "Dockerfile", "the name of the dockerfile to use")
	cmd.Flags().BoolVarP(&o.NoGit, "no-git", "", false, "disables adding of the generated .mink.yaml file to git")

	o.BaseOptions.AddBaseFlags(cmd)
	return cmd, o
}

// Run transforms the YAML files
func (o *Options) Run() error {
	chartDirs, err := o.findHelmChartDirs()
	if err != nil {
		return errors.Wrapf(err, "failed to find charts")
	}
	if len(chartDirs) == 0 {
		return nil
	}
	log.Logger().Debugf("found chart dirs %s", info(strings.Join(chartDirs, ", ")))

	image, err := o.findMinkImage()
	if err != nil {
		return errors.Wrapf(err, "failed to find mink image string")
	}
	if image == "" {
		return nil
	}
	log.Logger().Debugf("using image %s", info(image))

	// lets add an image ref to the first chart
	err = o.addImageToValuesFile(image, chartDirs[0])
	if err != nil {
		return errors.Wrapf(err, "failed to add image to values file")
	}

	minkFile := filepath.Join(o.Dir, minkFileName)
	exists, err := files.FileExists(minkFile)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", minkFile)
	}
	if exists {
		o.MinkEnabled = true
		return nil
	}
	err = o.createMinkFile(minkFile, chartDirs)
	if err != nil {
		return errors.Wrapf(err, "failed to ")
	}
	o.MinkEnabled = true

	if !o.NoGit {
		err = o.AddToGit(o.Dir, minkFileName)
		if err != nil {
			return errors.Wrapf(err, "failed to add mink file to git")
		}
	}
	return nil
}

func (o *Options) findHelmChartDirs() ([]string, error) {
	var dirs []string
	err := filepath.Walk(o.Dir, func(path string, info os.FileInfo, err error) error {
		if info == nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if name != "Chart.yaml" {
			return nil
		}
		dir := filepath.Dir(path)
		dirs = append(dirs, dir)
		return nil
	})
	if err != nil {
		return dirs, errors.Wrapf(err, "failed to find chart directories")
	}
	return dirs, nil
}

func (o *Options) createMinkFile(file string, dirs []string) error {
	buf := strings.Builder{}
	buf.WriteString("# the files containing a mink image build URI such as dockerfile:/// ko:/// or buildpack:///\n")
	buf.WriteString("filename:\n")
	for _, d := range dirs {
		buf.WriteString("- ")
		buf.WriteString(d)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")

	if kube.IsInCluster() {
		buf.WriteString("# if enabled invoke kaniko builds directly rather than using a separate TaskRun")
		buf.WriteString("local-kaniko: true")
	}
	buf.WriteString("\n")
	text := buf.String()

	err := ioutil.WriteFile(file, []byte(text), files.DefaultFileWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to save file %s", file)
	}
	log.Logger().Infof("created file %s", info(file))
	return nil
}

func (o *Options) findMinkImage() (string, error) {
	// check for a Dockerfile
	f := filepath.Join(o.Dir, o.Dockerfile)
	exists, err := files.FileExists(f)
	if err != nil {
		return "", errors.Wrapf(err, "failed to check if file exists %s", f)
	}
	if exists {
		return "dockerfile:///", nil
	}

	// check for build pack
	f = filepath.Join(o.Dir, "overrides.toml")
	exists, err = files.FileExists(f)
	if err != nil {
		return "", errors.Wrapf(err, "failed to check if file exists %s", f)
	}
	if exists {
		return "buildpack:///", nil
	}
	// TODO detect ko
	return "", nil
}

func (o *Options) addImageToValuesFile(image string, dir string) error {
	f := filepath.Join(dir, "values.yaml")
	exists, err := files.FileExists(f)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", f)
	}
	if !exists {
		return nil
	}

	node, err := yaml.ReadFile(f)
	if err != nil {
		return errors.Wrapf(err, "failed to load file %s", f)
	}

	v, err := node.Pipe(yaml.Lookup("image", "fullName"))
	if err != nil {
		return errors.Wrapf(err, "failed to lookup image.fullName")
	}
	if v != nil {
		text, err := v.String()
		if err != nil {
			return errors.Wrapf(err, "failed to get text for image.fullName")
		}
		if text == image {
			return nil
		}
	}

	err = node.PipeE(yaml.LookupCreate(yaml.ScalarNode, "image", "fullName"), yaml.FieldSetter{StringValue: image})
	if err != nil {
		return errors.Wrapf(err, "failed to set image.fullName to %s", image)
	}
	err = yaml.WriteFile(node, f)
	if err != nil {
		return errors.Wrapf(err, "failed to save file %s", f)
	}
	log.Logger().Infof("added image %s to file %s", info(image), info(f))
	return nil
}

// AddToGit adds the file to git in the given dir
func (o *Options) AddToGit(dir string, name string) error {
	r, err := git.PlainOpen(dir)
	if err != nil {
		return errors.Wrapf(err, "failed to open git dir %s", dir)
	}

	t, err := r.Worktree()
	if err != nil {
		return errors.Wrapf(err, "failed to get work tree")
	}

	_, err = t.Add(name)
	if err != nil {
		return errors.Wrapf(err, "failed to add file %s to git", name)
	}
	log.Logger().Infof("added file %s to git", info(name))
	return nil
}
