package tiltfile

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/pkg/errors"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tilt-dev/tilt/internal/tiltfile/links"

	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/tiltfile/io"
	tiltfile_k8s "github.com/tilt-dev/tilt/internal/tiltfile/k8s"
	"github.com/tilt-dev/tilt/internal/tiltfile/value"
	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"
	"github.com/tilt-dev/tilt/pkg/model"
)

var emptyYAMLError = fmt.Errorf("Empty YAML passed to k8s_yaml")

type referenceList []reference.Named

func (l referenceList) Len() int           { return len(l) }
func (l referenceList) Less(i, j int) bool { return l[i].String() < l[j].String() }
func (l referenceList) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }

type imageDepMetadata struct {
	required bool
	count    int
}

type k8sResource struct {
	// The name of this group, for display in the UX.
	name string

	// All k8s resources to be deployed.
	entities []k8s.K8sEntity

	imageRefs         referenceList
	imageDepsMetadata map[string]*imageDepMetadata

	portForwards []model.PortForward

	// labels for pods that we should watch and associate with this resource
	extraPodSelectors []labels.Set

	podReadinessMode model.PodReadinessMode

	discoveryStrategy v1alpha1.KubernetesDiscoveryStrategy

	imageMapDeps []string

	triggerMode triggerMode
	autoInit    bool

	resourceDeps []string

	manuallyGrouped bool

	links []model.Link

	labels map[string]string

	customDeploy *k8sCustomDeploy
}

// holds options passed to `k8s_resource` until assembly happens
type k8sResourceOptions struct {
	workload string
	// if non-empty, how to rename this resource
	newName           string
	portForwards      []model.PortForward
	extraPodSelectors []labels.Set
	triggerMode       triggerMode
	autoInit          value.Optional[starlark.Bool]
	tiltfilePosition  syntax.Position
	resourceDeps      []string
	objects           []string
	manuallyGrouped   bool
	podReadinessMode  model.PodReadinessMode
	discoveryStrategy v1alpha1.KubernetesDiscoveryStrategy
	links             []model.Link
	labels            map[string]string
}

// Count image injection for analytics.
func (r *k8sResource) imageRefInjectCounts() map[string]int {
	result := make(map[string]int, len(r.imageDepsMetadata))
	for key, value := range r.imageDepsMetadata {
		result[key] = value.count
	}
	return result
}

// Add a dependency on an image.
//
// Most image deps are optional. e.g., if you apply an nginx deployment,
// but don't build an nginx image, your cluster can pull the production
// nginx image. But if you want to use your own nginx image, you can specify one.
//
// But you can also specify required deps. e.g., a k8s_custom_deploy
// can declare that an image must be built locally and injected into the
// deploy command.
func (r *k8sResource) addImageDep(image reference.Named, required bool) {
	metadata, ok := r.imageDepsMetadata[image.String()]
	if !ok {
		r.imageRefs = append(r.imageRefs, image)

		metadata = &imageDepMetadata{}
		r.imageDepsMetadata[image.String()] = metadata
	}
	metadata.count++
	metadata.required = metadata.required || required
}

func (r *k8sResource) addEntities(entities []k8s.K8sEntity,
	locators []k8s.ImageLocator, envVarImages []container.RefSelector) error {
	r.entities = append(r.entities, entities...)

	for _, entity := range entities {
		images, err := entity.FindImages(locators, envVarImages)
		if err != nil {
			return errors.Wrapf(err, "finding image in %s/%s", entity.GVK().Kind, entity.Name())
		}
		for _, image := range images {
			r.addImageDep(image, false)
		}
	}

	return nil
}

func (s *tiltfileState) k8sYaml(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var yamlValue starlark.Value
	var allowDuplicates bool

	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"yaml", &yamlValue,
		"allow_duplicates?", &allowDuplicates,
	); err != nil {
		return nil, err
	}
	//normalize the starlark value into a slice
	value := starlarkValueOrSequenceToSlice(yamlValue)

	//if `None` was passed into k8s_yaml, len(val) = 0
	if len(value) > 0 {

		val, _ := starlark.AsString(value[0])
		entities, err := s.yamlEntitiesFromSkylarkValueOrList(thread, yamlValue)

		if err != nil {
			return nil, err
		}

		//the parameter blob('') results in an empty string
		if len(entities) == 0 && val == "" {
			return nil, emptyYAMLError
		}
		err = s.k8sObjectIndex.Append(thread, entities, allowDuplicates)
		if err != nil {
			return nil, err
		}

		s.k8sUnresourced = append(s.k8sUnresourced, entities...)

	} else {
		return nil, emptyYAMLError
	}

	return starlark.None, nil
}

func (s *tiltfileState) extractSecrets() model.SecretSet {
	result := model.SecretSet{}
	for _, e := range s.k8sUnresourced {
		secrets := s.maybeExtractSecrets(e)
		result.AddAll(secrets)
	}

	for _, k := range s.k8s {
		for _, e := range k.entities {
			secrets := s.maybeExtractSecrets(e)
			result.AddAll(secrets)
		}
	}
	return result
}

func (s *tiltfileState) maybeExtractSecrets(e k8s.K8sEntity) model.SecretSet {
	if !s.secretSettings.ScrubSecrets {
		// Secret scrubbing disabled, don't extract any secrets
		return nil
	}

	secret, ok := e.Obj.(*v1.Secret)
	if !ok {
		return nil
	}

	result := model.SecretSet{}
	for key, data := range secret.Data {
		result.AddSecret(secret.Name, key, data)
	}

	for key, data := range secret.StringData {
		result.AddSecret(secret.Name, key, []byte(data))
	}
	return result
}

func (s *tiltfileState) filterYaml(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var yamlValue starlark.Value
	var metaLabels value.StringStringMap
	var name, namespace, kind, apiVersion string
	err := s.unpackArgs(fn.Name(), args, kwargs,
		"yaml", &yamlValue,
		"labels?", &metaLabels,
		"name?", &name,
		"namespace?", &namespace,
		"kind?", &kind,
		"api_version?", &apiVersion,
	)
	if err != nil {
		return nil, err
	}

	entities, err := s.yamlEntitiesFromSkylarkValueOrList(thread, yamlValue)
	if err != nil {
		return nil, err
	}

	k, err := k8s.NewPartialMatchObjectSelector(apiVersion, kind, name, namespace)
	if err != nil {
		return nil, err
	}

	var match, rest []k8s.K8sEntity
	for _, e := range entities {
		if k.Matches(e) {
			match = append(match, e)
		} else {
			rest = append(rest, e)
		}
	}

	if len(metaLabels) > 0 {
		var r []k8s.K8sEntity
		match, r, err = k8s.FilterByMetadataLabels(match, metaLabels)
		if err != nil {
			return nil, err
		}
		rest = append(rest, r...)
	}

	matchingStr, err := k8s.SerializeSpecYAML(match)
	if err != nil {
		return nil, err
	}
	restStr, err := k8s.SerializeSpecYAML(rest)
	if err != nil {
		return nil, err
	}

	var source string
	switch y := yamlValue.(type) {
	case io.Blob:
		source = y.Source
	default:
		source = "filter_yaml"
	}

	return starlark.Tuple{
		io.NewBlob(matchingStr, source), io.NewBlob(restStr, source),
	}, nil
}

func (s *tiltfileState) k8sImageLocatorsList() []k8s.ImageLocator {
	locators := []k8s.ImageLocator{}
	for _, info := range s.k8sKinds {
		locators = append(locators, info.ImageLocators...)
	}
	return locators
}

func (s *tiltfileState) k8sResource(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var workload value.Name
	var newName value.Name
	var portForwardsVal starlark.Value
	var extraPodSelectorsVal starlark.Value
	var triggerMode triggerMode
	var resourceDepsVal starlark.Sequence
	var objectsVal starlark.Sequence
	var podReadinessMode tiltfile_k8s.PodReadinessMode
	var links links.LinkList
	var autoInit = value.Optional[starlark.Bool]{Value: true}
	var labels value.LabelSet
	var discoveryStrategy tiltfile_k8s.DiscoveryStrategy

	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"workload?", &workload,
		"new_name?", &newName,
		"port_forwards?", &portForwardsVal,
		"extra_pod_selectors?", &extraPodSelectorsVal,
		"trigger_mode?", &triggerMode,
		"resource_deps?", &resourceDepsVal,
		"objects?", &objectsVal,
		"auto_init?", &autoInit,
		"pod_readiness?", &podReadinessMode,
		"links?", &links,
		"labels?", &labels,
		"discovery_strategy?", &discoveryStrategy,
	); err != nil {
		return nil, err
	}

	resourceName := workload.String()
	manuallyGrouped := false
	if workload == "" {
		resourceName = newName.String()
		// If a resource doesn't specify an existing workload then it needs to have objects to be valid
		manuallyGrouped = true
	}

	if resourceName == "" {
		return nil, fmt.Errorf("Resource name missing. Must give a name for an existing resource or a new_name to create a new resource.")
	}

	portForwards, err := convertPortForwards(portForwardsVal)
	if err != nil {
		return nil, errors.Wrapf(err, "%s %q", fn.Name(), resourceName)
	}

	extraPodSelectors, err := podLabelsFromStarlarkValue(extraPodSelectorsVal)
	if err != nil {
		return nil, err
	}

	resourceDeps, err := value.SequenceToStringSlice(resourceDepsVal)
	if err != nil {
		return nil, errors.Wrapf(err, "%s: resource_deps", fn.Name())
	}

	objects, err := value.SequenceToStringSlice(objectsVal)
	if err != nil {
		return nil, errors.Wrapf(err, "%s: resource_deps", fn.Name())
	}

	if manuallyGrouped && len(objects) == 0 {
		return nil, fmt.Errorf("k8s_resource doesn't specify a workload or any objects. All non-workload resources must specify 1 or more objects")
	}

	labelMap := make(map[string]string)
	for k, v := range labels.Values {
		labelMap[k] = v
	}

	s.k8sResourceOptions = append(s.k8sResourceOptions, k8sResourceOptions{
		workload:          resourceName,
		newName:           string(newName),
		portForwards:      portForwards,
		extraPodSelectors: extraPodSelectors,
		tiltfilePosition:  thread.CallFrame(1).Pos,
		triggerMode:       triggerMode,
		autoInit:          autoInit,
		resourceDeps:      resourceDeps,
		objects:           objects,
		manuallyGrouped:   manuallyGrouped,
		podReadinessMode:  podReadinessMode.Value,
		links:             links.Links,
		labels:            labelMap,
		discoveryStrategy: v1alpha1.KubernetesDiscoveryStrategy(discoveryStrategy),
	})

	return starlark.None, nil
}

func labelSetFromStarlarkDict(d *starlark.Dict) (labels.Set, error) {
	ret := make(labels.Set)

	for _, t := range d.Items() {
		kVal := t[0]
		k, ok := kVal.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("pod label keys must be strings; got '%s' of type %T", kVal.String(), kVal)
		}
		vVal := t[1]
		v, ok := vVal.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("pod label values must be strings; got '%s' of type %T", vVal.String(), vVal)
		}
		ret[string(k)] = string(v)
	}
	if len(ret) > 0 {
		return ret, nil
	} else {
		return nil, nil
	}
}

func podLabelsFromStarlarkValue(v starlark.Value) ([]labels.Set, error) {
	if v == nil {
		return nil, nil
	}

	switch x := v.(type) {
	case *starlark.Dict:
		s, err := labelSetFromStarlarkDict(x)
		if err != nil {
			return nil, err
		} else if s == nil {
			return nil, nil
		} else {
			return []labels.Set{s}, nil
		}
	case *starlark.List:
		var ret []labels.Set

		it := x.Iterate()
		defer it.Done()
		var i starlark.Value
		for it.Next(&i) {
			d, ok := i.(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("pod labels elements must be dicts; got %T", i)
			}
			s, err := labelSetFromStarlarkDict(d)
			if err != nil {
				return nil, err
			} else if s != nil {
				ret = append(ret, s)
			}
		}

		return ret, nil
	default:
		return nil, fmt.Errorf("pod labels must be a dict or a list; got %T", v)
	}
}

func (s *tiltfileState) k8sImageJsonPath(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var apiVersion, kind, name, namespace string
	var locatorList tiltfile_k8s.JSONPathImageLocatorListSpec
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"paths", &locatorList,
		"api_version?", &apiVersion,
		"kind?", &kind,
		"name?", &name,
		"namespace?", &namespace,
	); err != nil {
		return nil, err
	}

	if kind == "" && name == "" && namespace == "" {
		return nil, errors.New("at least one of kind, name, or namespace must be specified")
	}

	k, err := k8s.NewPartialMatchObjectSelector(apiVersion, kind, name, namespace)
	if err != nil {
		return nil, err
	}

	paths, err := locatorList.ToImageLocators(k)
	if err != nil {
		return nil, err
	}

	kindInfo, ok := s.k8sKinds[k]
	if !ok {
		kindInfo = &tiltfile_k8s.KindInfo{}
		s.k8sKinds[k] = kindInfo
	}
	kindInfo.ImageLocators = paths

	return starlark.None, nil
}

func (s *tiltfileState) k8sKind(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	// require image_json_path to be passed as a kw arg since `k8s_kind("Environment", "{.foo.bar}")` feels confusing
	if len(args) > 1 {
		return nil, fmt.Errorf("%s: got %d arguments, want at most %d", fn.Name(), len(args), 1)
	}

	var apiVersion, kind string
	var jpLocators tiltfile_k8s.JSONPathImageLocatorListSpec
	var jpObjectLocator tiltfile_k8s.JSONPathImageObjectLocatorSpec
	var podReadiness tiltfile_k8s.PodReadinessMode
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"kind", &kind,
		"image_json_path?", &jpLocators,
		"api_version?", &apiVersion,
		"image_object?", &jpObjectLocator,
		"pod_readiness?", &podReadiness,
	); err != nil {
		return nil, err
	}

	k, err := k8s.NewPartialMatchObjectSelector(apiVersion, kind, "", "")
	if err != nil {
		return nil, err
	}

	if !jpLocators.IsEmpty() && !jpObjectLocator.IsEmpty() {
		return nil, fmt.Errorf("Cannot specify both image_json_path and image_object")
	}

	kindInfo, ok := s.k8sKinds[k]
	if !ok {
		kindInfo = &tiltfile_k8s.KindInfo{}
		s.k8sKinds[k] = kindInfo
	}

	if !jpLocators.IsEmpty() {
		locators, err := jpLocators.ToImageLocators(k)
		if err != nil {
			return nil, err
		}

		kindInfo.ImageLocators = locators
	} else if !jpObjectLocator.IsEmpty() {
		locator, err := jpObjectLocator.ToImageLocator(k)
		if err != nil {
			return nil, err
		}
		kindInfo.ImageLocators = []k8s.ImageLocator{locator}
	}

	if podReadiness.Value != "" {
		kindInfo.PodReadinessMode = podReadiness.Value
	}

	return starlark.None, nil
}

func (s *tiltfileState) workloadToResourceFunctionFn(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var wtrf *starlark.Function
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"func", &wtrf); err != nil {
		return nil, err
	}

	workloadToResourceFunction, err := makeWorkloadToResourceFunction(wtrf)
	if err != nil {
		return starlark.None, err
	}

	s.workloadToResourceFunction = workloadToResourceFunction

	return starlark.None, nil
}

type k8sObjectID struct {
	name      string
	kind      string
	namespace string
	group     string
}

func (k k8sObjectID) Attr(name string) (starlark.Value, error) {
	switch name {
	case "name":
		return starlark.String(k.name), nil
	case "kind":
		return starlark.String(k.kind), nil
	case "namespace":
		return starlark.String(k.namespace), nil
	case "group":
		return starlark.String(k.group), nil
	default:
		return starlark.None, fmt.Errorf("%T has no attribute '%s'", k, name)
	}
}

func (k k8sObjectID) AttrNames() []string {
	return []string{"name", "kind", "namespace", "group"}
}

func (k k8sObjectID) String() string {
	return strings.ToLower(fmt.Sprintf("%s:%s:%s:%s", k.name, k.kind, k.namespace, k.group))
}

func (k k8sObjectID) Type() string {
	return "K8sObjectID"
}

func (k k8sObjectID) Freeze() {
}

func (k k8sObjectID) Truth() starlark.Bool {
	return k.name != "" || k.kind != "" || k.namespace != "" || k.group != ""
}

func (k k8sObjectID) Hash() (uint32, error) {
	return starlark.Tuple{starlark.String(k.name), starlark.String(k.kind), starlark.String(k.namespace), starlark.String(k.group)}.Hash()
}

var _ starlark.Value = k8sObjectID{}

type workloadToResourceFunction struct {
	fn  func(thread *starlark.Thread, id k8sObjectID) (string, error)
	pos syntax.Position
}

func makeWorkloadToResourceFunction(f *starlark.Function) (workloadToResourceFunction, error) {
	if f.NumParams() != 1 {
		return workloadToResourceFunction{}, fmt.Errorf("%s arg must take 1 argument. %s takes %d", workloadToResourceFunctionN, f.Name(), f.NumParams())
	}
	fn := func(thread *starlark.Thread, id k8sObjectID) (string, error) {
		ret, err := starlark.Call(thread, f, starlark.Tuple{id}, nil)
		if err != nil {
			return "", err
		}
		s, ok := ret.(starlark.String)
		if !ok {
			return "", fmt.Errorf("%s: invalid return value. wanted: string. got: %T", f.Name(), ret)
		}
		return string(s), nil
	}

	return workloadToResourceFunction{
		fn:  fn,
		pos: f.Position(),
	}, nil
}

func (s *tiltfileState) checkResourceConflict(name string) error {
	if s.k8sByName[name] != nil {
		return fmt.Errorf("k8s_resource named %q already exists", name)
	}
	if s.localByName[name] != nil {
		return fmt.Errorf("local_resource named %q already exists", name)
	}
	if s.dcByName[name] != nil {
		return fmt.Errorf("dc_resource named %q already exists", name)
	}
	return nil
}

func (s *tiltfileState) makeK8sResource(name string) (*k8sResource, error) {
	err := s.checkResourceConflict(name)
	if err != nil {
		return nil, err
	}

	r := &k8sResource{
		name:              name,
		imageDepsMetadata: make(map[string]*imageDepMetadata),
		autoInit:          true,
		labels:            make(map[string]string),
	}
	s.k8s = append(s.k8s, r)
	s.k8sByName[name] = r

	return r, nil
}

func (s *tiltfileState) yamlEntitiesFromSkylarkValueOrList(thread *starlark.Thread, v starlark.Value) ([]k8s.K8sEntity, error) {
	values := starlarkValueOrSequenceToSlice(v)

	var ret []k8s.K8sEntity

	for _, value := range values {
		entities, err := s.yamlEntitiesFromSkylarkValue(thread, value)
		if err != nil {
			return nil, err
		}
		ret = append(ret, entities...)
	}

	return ret, nil
}

func parseYAMLFromBlob(blob io.Blob) ([]k8s.K8sEntity, error) {
	ret, err := k8s.ParseYAMLFromString(blob.String())
	if err != nil {
		return nil, errors.Wrapf(err, "Error reading yaml from %s", blob.Source)
	}
	return ret, nil
}

func (s *tiltfileState) yamlEntitiesFromSkylarkValue(thread *starlark.Thread, v starlark.Value) ([]k8s.K8sEntity, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case io.Blob:
		return parseYAMLFromBlob(v)
	default:
		yamlPath, err := value.ValueToAbsPath(thread, v)
		if err != nil {
			return nil, err
		}
		bs, err := io.ReadFile(thread, yamlPath)
		if err != nil {
			return nil, errors.Wrap(err, "error reading yaml file")
		}

		entities, err := k8s.ParseYAMLFromString(string(bs))
		if err != nil {
			if strings.Contains(err.Error(), "json parse error: ") {
				return entities, fmt.Errorf("%s is not a valid YAML file: %s", yamlPath, err)
			}
			return entities, err
		}

		return entities, nil
	}
}

func convertPortForwards(val starlark.Value) ([]model.PortForward, error) {
	if val == nil {
		return nil, nil
	}
	switch val := val.(type) {
	case starlark.NoneType:
		return nil, nil

	case starlark.Int:
		pf, err := intToPortForward(val)
		if err != nil {
			return nil, err
		}
		return []model.PortForward{pf}, nil

	case starlark.String:
		pf, err := stringToPortForward(val)
		if err != nil {
			return nil, err
		}
		return []model.PortForward{pf}, nil

	case portForward:
		return []model.PortForward{val.PortForward}, nil
	case starlark.Sequence:
		var result []model.PortForward
		it := val.Iterate()
		defer it.Done()
		var i starlark.Value
		for it.Next(&i) {
			switch i := i.(type) {
			case starlark.Int:
				pf, err := intToPortForward(i)
				if err != nil {
					return nil, err
				}
				result = append(result, pf)

			case starlark.String:
				pf, err := stringToPortForward(i)
				if err != nil {
					return nil, err
				}
				result = append(result, pf)

			case portForward:
				result = append(result, i.PortForward)
			default:
				return nil, fmt.Errorf("port_forwards arg %v includes element %v which must be an int or a port_forward; is a %T", val, i, i)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("port_forwards must be an int, a port_forward, or a sequence of those; is a %T", val)
	}
}

func (s *tiltfileState) portForward(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var local, container int
	var name, path, host string

	// TODO: can specify host (see `stringToPortForward` for host validation logic)
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"local_port", &local,
		"container_port?", &container,
		"name?", &name,
		"link_path?", &path,
		"host?", &host); err != nil {
		return nil, err
	}

	var parsedPath *url.URL
	if path != "" {
		var err error
		parsedPath, err = url.Parse(path)
		if err != nil {
			return portForward{}, errors.Wrapf(err, "parsing `path` param")
		}
	}
	return portForward{
		model.PortForward{LocalPort: local, ContainerPort: container, Host: host, Name: name}.WithPath(parsedPath),
	}, nil
}

type portForward struct {
	model.PortForward
}

var _ starlark.Value = portForward{}

func (f portForward) String() string {
	return fmt.Sprintf("port_forward(local_port=%d, container_port=%d, name=%q)",
		f.LocalPort, f.ContainerPort, f.Name)
}

func (f portForward) Type() string {
	return "port_forward"
}

func (f portForward) Freeze() {}

func (f portForward) Truth() starlark.Bool {
	return f.PortForward != model.PortForward{}
}

func (f portForward) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: port_forward")
}

func intToPortForward(i starlark.Int) (model.PortForward, error) {
	n, ok := i.Int64()
	if !ok {
		return model.PortForward{}, fmt.Errorf("portForward port value %v is not representable as an int64", i)
	}
	if n < 0 || n > 65535 {
		return model.PortForward{}, fmt.Errorf("portForward port value %v is not in the valid range [0-65535]", n)
	}
	return model.PortForward{LocalPort: int(n)}, nil
}

const ipReStr = `^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$`
const hostnameReStr = `^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9])$`

var validHost = regexp.MustCompile(ipReStr + "|" + hostnameReStr)

func stringToPortForward(s starlark.String) (model.PortForward, error) {
	parts := strings.SplitN(string(s), ":", 3)

	var host string
	var localString string
	if len(parts) == 3 {
		localString = parts[1]
		host = parts[0]
		if !validHost.MatchString(host) {
			return model.PortForward{}, fmt.Errorf("portForward host value %q is not a valid hostname or IP address", localString)
		}
	} else {
		localString = parts[0]
	}

	local, err := strconv.Atoi(localString)
	if err != nil || local < 0 || local > 65535 {
		return model.PortForward{}, fmt.Errorf("portForward port value %q is not in the valid range [0-65535]", localString)
	}

	var container int
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		container, err = strconv.Atoi(last)
		if err != nil || container < 0 || container > 65535 {
			return model.PortForward{}, fmt.Errorf("portForward port value %q is not in the valid range [0-65535]", last)
		}
	}
	return model.PortForward{LocalPort: local, ContainerPort: container, Host: host}, nil
}

func (s *tiltfileState) calculateResourceNames(workloads []k8s.K8sEntity) ([]string, error) {
	if s.workloadToResourceFunction.fn != nil {
		names, err := s.workloadToResourceFunctionNames(workloads)
		if err != nil {
			return nil, errors.Wrapf(err, "%s: error applying workload_to_resource_function", s.workloadToResourceFunction.pos.String())
		}
		return names, nil
	} else {
		return k8s.UniqueNames(workloads, 1), nil
	}
}

// calculates names for workloads using s.workloadToResourceFunction
func (s *tiltfileState) workloadToResourceFunctionNames(workloads []k8s.K8sEntity) ([]string, error) {
	takenNames := make(map[string]k8s.K8sEntity)
	ret := make([]string, len(workloads))
	thread := &starlark.Thread{
		Print: s.print,
	}
	for i, e := range workloads {
		id := newK8sObjectID(e)
		name, err := s.workloadToResourceFunction.fn(thread, id)
		if err != nil {
			return nil, errors.Wrapf(err, "error determining resource name for '%s'", id.String())
		}

		if conflictingWorkload, ok := takenNames[name]; ok {
			return nil, fmt.Errorf("both '%s' and '%s' mapped to resource name '%s'", newK8sObjectID(e).String(), newK8sObjectID(conflictingWorkload).String(), name)
		}

		ret[i] = name
		takenNames[name] = e
	}
	return ret, nil
}

func newK8sObjectID(e k8s.K8sEntity) k8sObjectID {
	gvk := e.GVK()
	return k8sObjectID{
		name:      e.Name(),
		kind:      gvk.Kind,
		namespace: e.Namespace().String(),
		group:     gvk.Group,
	}
}
