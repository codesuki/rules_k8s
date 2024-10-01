package resolver

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	"github.com/bazelbuild/rules_docker/container/go/pkg/compat"
	"github.com/bazelbuild/rules_docker/container/go/pkg/utils"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"gopkg.in/yaml.v2"
)

// Flags defines the flags that rules_k8s may pass to the resolver
type Flags struct {
	ImgChroot         string
	K8sTemplate       string
	SubstitutionsFile string
	AllowUnusedImages bool
	NoPush            bool
	StampInfoFile     utils.ArrayStringFlags
	ImgSpecs          utils.ArrayStringFlags
	OCIImages         utils.ArrayStringFlags
}

// Commandline flags
const (
	FlagImgChroot         = "image_chroot"
	FlagK8sTemplate       = "template"
	FlagSubstitutionsFile = "substitutions"
	FlagAllowUnusedImages = "allow_unused_images"
	FlagNoPush            = "no_push"
	FlagImgSpecs          = "image_spec"
	FlagStampInfoFile     = "stamp-info-file"
	FlagOCIImages         = "oci_image"
)

// RegisterFlags will register the resolvers flags with the provided FlagSet.
// It returns a struct that will contain the values once flags are parsed.
// The caller is responsible for parsing flags when ready.
func RegisterFlags(flagset *flag.FlagSet) *Flags {
	var flags Flags

	flagset.StringVar(&flags.ImgChroot, FlagImgChroot, "", "The repository under which to chroot image references when publishing them.")
	flagset.StringVar(&flags.K8sTemplate, FlagK8sTemplate, "", "The k8s YAML template file to resolve.")
	flagset.StringVar(&flags.SubstitutionsFile, FlagSubstitutionsFile, "", "A file with a list of substitutions that were made in the YAML template. Any stamp values that appear are stamped by the resolver.")
	flagset.BoolVar(&flags.AllowUnusedImages, FlagAllowUnusedImages, false, "Allow images that don't appear in the JSON. This is useful when generating multiple SKUs of a k8s_object, only some of which use a particular image.")
	flagset.BoolVar(&flags.NoPush, FlagNoPush, false, "Don't push images after resolving digests.")
	flagset.Var(&flags.ImgSpecs, FlagImgSpecs, "Associative lists of the constituent elements of a docker image.")
	flagset.Var(&flags.StampInfoFile, FlagStampInfoFile, "One or more Bazel stamp info files.")
	flagset.Var(&flags.OCIImages, FlagOCIImages, "Associative lists of constituent elements of an OCI image.")
	return &flags
}

// Option can be passed to NewResolver to configure the resolver.
type Option struct {
	apply func(r *Resolver)
}

// Resolver performs substitutions and resolves/pushes images
type Resolver struct {
	flags *Flags

	// parseTag is called instead of name.NewTag, which allows overriding how image
	// tags are parsed.
	parseTag func(name string, opts ...name.Option) (name.Tag, error)
}

// ParseTagOption specifies a function to be used instead of name.NewTag to parse image tags.
//
// This option allows specifying different name.Option values in the call to name.Tag. For
// example, the default logic for detecting whether a registry name is insecure can be
// overridden.
func ParseTagOption(f func(name string, opts ...name.Option) (name.Tag, error)) Option {
	return Option{
		func(r *Resolver) { r.parseTag = f },
	}
}

// NewResolver takes some Flags and returns a Resolver
func NewResolver(flags *Flags, option ...Option) *Resolver {
	r := &Resolver{
		flags:    flags,
		parseTag: name.NewTag,
	}
	for _, o := range option {
		o.apply(r)
	}
	return r
}

// Resolve will parse the files pointed by the flags and return a resolvedTemplate and error as applicable
func (r *Resolver) Resolve() (resolvedTemplate string, err error) {
	stamper, err := compat.NewStamper(r.flags.StampInfoFile)
	if err != nil {
		return "", fmt.Errorf("Failed to initialize the stamper: %w", err)
	}

	if len(r.flags.ImgSpecs) != 0 && len(r.flags.OCIImages) != 0 {
		return "", fmt.Errorf("Can only resolve either Docker or OCI images")
	}

	substitutions := map[string]string{}
	if r.flags.SubstitutionsFile != "" {
		substitutions, err = parseSubstitutions(r.flags.SubstitutionsFile, stamper)
		if err != nil {
			return "", fmt.Errorf("Unable to parse substitutions file %s: %w", r.flags.SubstitutionsFile, err)
		}
	}
	resolvedImages := map[string]string{}
	unseen := map[string]bool{}
	if len(r.flags.ImgSpecs) > 0 {
		specs := []imageSpec{}
		for _, s := range r.flags.ImgSpecs {
			spec, err := parseImageSpec(s)
			if err != nil {
				return "", fmt.Errorf("Unable to parse image spec %q: %s", s, err)
			}
			specs = append(specs, spec)
		}

		var err error
		resolvedImages, unseen, err = r.publish(specs, stamper)
		if err != nil {
			return "", fmt.Errorf("Unable to publish images: %w", err)
		}
	}

	// new code path
	if len(r.flags.OCIImages) > 0 {
		specs := []ociSpec{}
		for _, s := range r.flags.OCIImages {
			spec, err := parseOCISpec(s)
			if err != nil {
				return "", fmt.Errorf("Unable to parse image spec %q: %s", s, err)
			}
			specs = append(specs, spec)
		}

		for _, s := range specs {

			stampedName := stamper.Stamp(s.name)
			ref, err := r.parseTag(stampedName, name.WeakValidation)
			if err != nil {
				return "", fmt.Errorf("unable to create a docker tag from stamped name %q: %v", stampedName, err)
			}

			desc, err := s.ImageDescriptor()

			resolvedImages[s.name] = fmt.Sprintf("%s/%s@%v", ref.Context().RegistryStr(), ref.Context().RepositoryStr(), desc.Manifests[0].Digest)
		}
	}

	resolvedTemplate, err = resolveTemplate(r.flags.K8sTemplate, resolvedImages, unseen, substitutions)
	if err != nil {
		return resolvedTemplate, fmt.Errorf("Unable to resolve template file %q: %w", r.flags.K8sTemplate, err)
	}
	if len(unseen) > 0 && !r.flags.AllowUnusedImages {
		log.Printf("The following images given as --image_spec were not found in the template:")
		for i := range unseen {
			log.Printf("%s", i)
		}
		return resolvedTemplate, fmt.Errorf("--allow_unused_images can be specified to ignore this error.")
	}
	return
}

type imageDescriptor struct {
	SchemaVersion int
	MediaType     string
	Manifests     []struct {
		MediaType string
		Digest    string
		Size      int
	}
}

type ociSpec struct {
	name      string
	directory string
}

func (s *ociSpec) ImageDescriptor() (*imageDescriptor, error) {
	// Read digest from index.json
	/*
		{
		  "schemaVersion": 2,
		  "mediaType": "application/vnd.oci.image.index.v1+json",
		  "manifests": [
		    {
		      "mediaType": "application/vnd.oci.image.manifest.v1+json",
		      "digest": "sha256:3f1e8f6138a66607f7eb17c072795145b86878fc75687b05a23f793a3ab206cd",
		      "size": 2555
		    }
		  ]
		}
	*/
	path := fmt.Sprintf("%s/index.json", s.directory)
	data, err := os.ReadFile(path)
	if err != nil {
		return &imageDescriptor{}, fmt.Errorf("could not read index.json: %w", err)
	}
	d := &imageDescriptor{}
	err = json.Unmarshal(data, &d)
	if err != nil {
		return &imageDescriptor{}, fmt.Errorf("could not unmarshal index.json: %w", err)
	}
	return d, validateImageDescriptor(d)
}

func validateImageDescriptor(d *imageDescriptor) error {
	if d.SchemaVersion != 2 {
		return fmt.Errorf("expected SchemaVersion to be '2'")
	}
	if d.MediaType != "application/vnd.oci.image.index.v1+json" {
		return fmt.Errorf("Expected MediaType to be 'application/vnd.oci.image.index.v1+json'")
	}
	if len(d.Manifests) == 0 {
		return fmt.Errorf("No manifests found")
	}
	if d.Manifests[0].MediaType != "application/vnd.oci.image.manifest.v1+json" {
		return fmt.Errorf("Expected manifest MediaType to be 'application/vnd.oci.image.manifest.v1+json'")
	}
	if d.Manifests[0].Digest == "" {
		return fmt.Errorf("Manifest digest is empty")
	}
	return nil
}

func parseOCISpec(spec string) (ociSpec, error) {
	result := ociSpec{}
	splitSpec := strings.Split(spec, ";")
	for _, s := range splitSpec {
		splitFields := strings.SplitN(s, "=", 2)
		if len(splitFields) != 2 {
			return ociSpec{}, fmt.Errorf("image spec item %q split by '=' into unexpected fields, got %d, want 2", s, len(splitFields))
		}
		switch splitFields[0] {
		case "name":
			result.name = splitFields[1]
		case "directory":
			result.directory = splitFields[1]
		default:
			return ociSpec{}, fmt.Errorf("unknown oci spec field %q", splitFields[0])
		}
	}
	return result, nil
}

// imageSpec describes the differents parts of an image generated by
// rules_docker.
type imageSpec struct {
	// name is the name of the image.
	name string
	// imgTarball is the image in the `docker save` tarball format.
	imgTarball string
	// imgConfig if the config JSON file of the image.
	imgConfig string
	// digests is a list of files with the sha256 digests of the compressed
	// layers.
	digests []string
	// diffIDs is a list of files with the sha256 digests of the uncompressed
	// layers.
	diffIDs []string
	// compressedLayers are the paths to the compressed layer tarballs.
	compressedLayers []string
	// uncompressedLayers are the paths to the uncompressed layer tarballs.
	uncomressedLayers []string
}

// layers returns a list of strings that can be passed to the image reader in
// the compatiblity package of rules_docker to read the layers of an image in
// the format "va11,val2,val3,val4" where:
// val1 is the compressed layer tarball.
// val2 is the uncompressed layer tarball.
// val3 is the digest file.
// val4 is the diffID file.
func (s *imageSpec) layers() ([]string, error) {
	result := []string{}
	if len(s.digests) != len(s.diffIDs) || len(s.diffIDs) != len(s.compressedLayers) || len(s.compressedLayers) != len(s.uncomressedLayers) {
		return nil, fmt.Errorf("digest, diffID, compressed blobs & uncompressed blobs had unequal lengths for image %s, got %d, %d, %d, %d, want all of the lengths to be equal", s.name, len(s.digests), len(s.diffIDs), len(s.compressedLayers), len(s.uncomressedLayers))
	}
	for i, digest := range s.digests {
		diffID := s.diffIDs[i]
		compressedLayer := s.compressedLayers[i]
		uncompressedLayer := s.uncomressedLayers[i]
		result = append(result, fmt.Sprintf("%s,%s,%s,%s", compressedLayer, uncompressedLayer, digest, diffID))
	}
	return result, nil
}

// parseImageSpec parses the differents parts of a single docker image specified
// as string in the format "key1=val1;key2=val2" where the expected keys are:
// 1. "name": Name of the image.
// 2. "tarball": docker save tarball of the image.
// 3. "config": JSON config file of the image.
// 4. "diff_id": Files with sha256 digest of uncompressed layers.
// 5. "digest": Files with sha256 digest of compressed layers.
// 6. "compressed_layer": Path to compressed layer tarballs.
// 7. "uncompressed_layer": Path to uncompressed layer tarballs.
func parseImageSpec(spec string) (imageSpec, error) {
	result := imageSpec{}
	splitSpec := strings.Split(spec, ";")
	for _, s := range splitSpec {
		splitFields := strings.SplitN(s, "=", 2)
		if len(splitFields) != 2 {
			return imageSpec{}, fmt.Errorf("image spec item %q split by '=' into unexpected fields, got %d, want 2", s, len(splitFields))
		}
		switch splitFields[0] {
		case "name":
			result.name = splitFields[1]
		case "tarball":
			result.imgTarball = splitFields[1]
		case "config":
			result.imgConfig = splitFields[1]
		case "diff_id":
			result.diffIDs = strings.Split(splitFields[1], ",")
		case "digest":
			result.digests = strings.Split(splitFields[1], ",")
		case "compressed_layer":
			result.compressedLayers = strings.Split(splitFields[1], ",")
		case "uncompressed_layer":
			result.uncomressedLayers = strings.Split(splitFields[1], ",")
		default:
			return imageSpec{}, fmt.Errorf("unknown image spec field %q", splitFields[0])
		}
	}
	return result, nil
}

// parseSubsitutions parses a substitution file, which should be a JSON object
// with strings to search for and values to replace them with. The replacement values
// are stamped using the provided stamper.
func parseSubstitutions(file string, stamper *compat.Stamper) (map[string]string, error) {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("unable to read file: %v", err)
	}

	result := struct {
		Substitutions map[string]string
	}{}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, fmt.Errorf("unmarshaling as JSON: %v", err)
	}

	for k, v := range result.Substitutions {
		result.Substitutions[k] = stamper.Stamp(v)
	}

	return result.Substitutions, nil
}

// publishSingle publishes a docker image with the given spec to the remote
// registry indicated in the image name. The image name is stamped with the
// given stamper.
// The stamped image name is returned referenced by its sha256 digest.
func (r *Resolver) publishSingle(spec imageSpec, stamper *compat.Stamper) (string, error) {
	layers, err := spec.layers()
	if err != nil {
		return "", fmt.Errorf("unable to convert the layer parts in image spec for %s into a single comma separated argument: %v", spec.name, err)
	}

	imgParts, err := compat.ImagePartsFromArgs(spec.imgConfig, "", spec.imgTarball, layers)
	if err != nil {
		return "", fmt.Errorf("unable to determine parts of the image from the specified arguments: %v", err)
	}
	cr := compat.Reader{Parts: imgParts}
	img, err := cr.ReadImage()
	if err != nil {
		return "", fmt.Errorf("error reading image: %v", err)
	}
	stampedName := stamper.Stamp(spec.name)

	var ref name.Reference
	if r.flags.ImgChroot != "" {
		n := path.Join(r.flags.ImgChroot, stampedName)
		t, err := r.parseTag(n, name.WeakValidation)
		if err != nil {
			return "", fmt.Errorf("unable to create a docker tag from stamped name %q: %v", n, err)
		}
		ref = t
	} else {
		t, err := r.parseTag(stampedName, name.WeakValidation)
		if err != nil {
			return "", fmt.Errorf("unable to create a docker tag from stamped name %q: %v", stampedName, err)
		}
		ref = t
	}
	auth, err := authn.DefaultKeychain.Resolve(ref.Context())
	if err != nil {
		return "", fmt.Errorf("unable to get authenticator for image %v", ref.Name())
	}

	if !r.flags.NoPush {
		if err := remote.Write(ref, img, remote.WithAuth(auth)); err != nil {
			return "", fmt.Errorf("unable to push image %v: %v", ref.Name(), err)
		}
	}

	d, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("unable to get digest of image %v", ref.Name())
	}

	return fmt.Sprintf("%s/%s@%v", ref.Context().RegistryStr(), ref.Context().RepositoryStr(), d), nil
}

// publish publishes the image with the given spec. It returns:
// 1. A map from the unstamped & tagged image name to the stamped image name
//    referenced by its sha256 digest.
// 2. A set of unstamped & tagged image names that were pushed to the registry.
func (r *Resolver) publish(spec []imageSpec, stamper *compat.Stamper) (map[string]string, map[string]bool, error) {
	overrides := make(map[string]string)
	unseen := make(map[string]bool)
	for _, s := range spec {
		digestRef, err := r.publishSingle(s, stamper)
		if err != nil {
			return nil, nil, err
		}
		overrides[s.name] = digestRef
		unseen[s.name] = true
	}
	return overrides, unseen, nil
}

// yamlResolver implements walking over arbitrary k8s YAML templates and
// transforming every string in the YAML with a configured string resolver.
type yamlResolver struct {
	// resolvedImages is a map from the tagged image name to the fully qualified
	// image name by sha256 digest.
	resolvedImages map[string]string
	// unseen is the set of images that haven't been seen yet. Image names
	// encountered in the k8s YAML template are removed from this set.
	unseen map[string]bool
	// strResolver is called to resolve every individual string encountered in
	// the k8s YAML template. The functor interface allows mocking the string
	// resolver in unit tests.
	strResolver func(*yamlResolver, string) (string, error)
	// numDocs stores the number of documents the resolver worked on when
	// resolveYAML was called. This is used for testing only.
	numDocs int
}

// resolveString resolves a string found in the k8s YAML template by replacing
// a tagged image name with an image name referenced by its sha256 digest. If
// the given string doesn't represent a tagged image, it is returned as is.
// The given resolver is also modified:
// 1. If the given string was a tagged image, the resolved image lookup in the
//    given resolver is updated to include a mapping from the given string to
//    the resolved image name.
// 2. If the given string was a tagged image, the set of unseen images in the
//    given resolver is updated to exclude the given string.
// The resolver is best-effort, i.e., if any errors are encountered, the given
// string is returned as is.
func resolveString(r *yamlResolver, s string) (string, error) {
	if _, ok := r.unseen[s]; ok {
		delete(r.unseen, s)
	}
	o, ok := r.resolvedImages[s]
	if ok {
		return o, nil
	}
	t, err := name.NewTag(s, name.StrictValidation)
	if err != nil {
		return s, nil
	}
	auth, err := authn.DefaultKeychain.Resolve(t.Context())
	if err != nil {
		return s, nil
	}
	desc, err := remote.Get(t, remote.WithAuth(auth))
	if err != nil {
		return s, nil
	}
	resolved := fmt.Sprintf("%s/%s@%v", t.Context().RegistryStr(), t.Context().RepositoryStr(), desc.Digest)
	r.resolvedImages[s] = resolved
	return resolved, nil
}

// resolveItem resolves the given YAML object if it's a string or recursively
// walks into the YAML collection type.
func (r *yamlResolver) resolveItem(i interface{}) (interface{}, error) {
	if s, ok := i.(string); ok {
		return r.strResolver(r, s)
	}
	if l, ok := i.([]interface{}); ok {
		return r.resolveList(l)
	}
	if m, ok := i.(map[interface{}]interface{}); ok {
		return r.resolveMap(m)
	}
	return i, nil
}

// resolveList recursively walks the given yaml list.
func (r *yamlResolver) resolveList(l []interface{}) ([]interface{}, error) {
	result := []interface{}{}
	for _, i := range l {
		o, err := r.resolveItem(i)
		if err != nil {
			return nil, fmt.Errorf("error resolving item %v in list: %v", i, err)
		}
		result = append(result, o)
	}
	return result, nil
}

// resolveMap recursively walks the given yaml map.
func (r *yamlResolver) resolveMap(m map[interface{}]interface{}) (map[interface{}]interface{}, error) {
	result := make(map[interface{}]interface{})
	for k, v := range m {
		rk, err := r.resolveItem(k)
		if err != nil {
			return nil, fmt.Errorf("error resolving key %v in map: %v", k, err)
		}
		rv, err := r.resolveItem(v)
		if err != nil {
			return nil, fmt.Errorf("error resolving value %v in map: %v", v, err)
		}
		result[rk] = rv
	}
	return result, nil
}

// yamlDoc implements the yaml.Unmarshaler interface that allows decoding an
// arbitrary YAML document.
type yamlDoc struct {
	// vList stores an arbitrary YAML list.
	vList []interface{}
	// vMap stores an arbitrary YAML map.
	vMap map[interface{}]interface{}
	// isInt stores whether this YAML document stores an integer.
	isInt bool
	// vInt stores a YAML integer.
	vInt int
	// isBool stores whether this YAML document stores a boolean.
	isBool bool
	// vBool stores a YAML boolean.
	vBool bool
	// isStr stores whether this YAML document stores a string.
	isStr bool
	// vStr stores a YAML string.
	vStr string
}

// UnmarshalYAML loads an arbitrary YAML document which can be a YAML list or
// a YAML map into the given YAML document.
func (y *yamlDoc) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal(&y.vList); err == nil {
		return nil
	}
	if err := unmarshal(&y.vMap); err == nil {
		return nil
	}
	if err := unmarshal(&y.vInt); err == nil {
		y.isInt = true
		return nil
	}
	if err := unmarshal(&y.vBool); err == nil {
		y.isBool = true
		return nil
	}
	if err := unmarshal(&y.vStr); err == nil {
		y.isStr = true
		return nil
	}
	return fmt.Errorf("unable to parse given blob as a YAML list, map or string, integer or boolean")
}

// val gets the stored YAML value in this document.
func (y *yamlDoc) val() interface{} {
	if y.vList != nil {
		return y.vList
	}
	if y.vMap != nil {
		return y.vMap
	}
	if y.isInt {
		return y.vInt
	}
	if y.isBool {
		return y.vBool
	}
	if y.isStr {
		return y.vStr
	}
	return nil
}

// resolveYAML recursively walks the given stream of arbitrary YAML documents
// and calls the strResolver on each string in the YAML document.
func (r *yamlResolver) resolveYAML(t io.Reader) ([]byte, error) {
	d := yaml.NewDecoder(t)
	buf := bytes.NewBuffer(nil)
	e := yaml.NewEncoder(buf)
	defer e.Close()
	for {
		y := yamlDoc{}
		err := d.Decode(&y)
		if err != nil && err != io.EOF {
			return nil, err
		}
		done := err == io.EOF
		o, err := r.resolveItem(y.val())
		if err != nil {
			return nil, fmt.Errorf("error resolving YAML template: %v", err)
		}
		if o != nil {
			r.numDocs++
			err = e.Encode(o)
			if err != nil {
				return nil, err
			}
		}
		if done {
			break
		}
	}

	return buf.Bytes(), nil
}

// resolveTemplate resolves the given YAML template using the given mapping from
// tagged to fully qualified image names referenced by their digest and the
// set of image names that haven't been seen yet. The given set of unseen images
// is updated to exclude the image names encountered in the given template. The
// given substitutions are made in the template.
func resolveTemplate(templateFile string, resolvedImages map[string]string, unseen map[string]bool, substitutions map[string]string) (string, error) {
	t, err := ioutil.ReadFile(templateFile)
	if err != nil {
		return "", fmt.Errorf("unable to read template file %q: %v", templateFile, err)
	}

	for k, v := range substitutions {
		t = bytes.ReplaceAll(t, []byte(k), []byte(v))
	}

	r := yamlResolver{
		resolvedImages: resolvedImages,
		unseen:         unseen,
		strResolver:    resolveString,
	}

	resolved, err := r.resolveYAML(bytes.NewReader(t))
	if err != nil {
		return "", fmt.Errorf("unable to resolve YAML template %q: %v", templateFile, err)
	}
	return string(resolved), nil
}
