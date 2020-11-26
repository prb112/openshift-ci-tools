package steps

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	coreapi "k8s.io/api/core/v1"
	rbacapi "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/junit"
	"github.com/openshift/ci-tools/pkg/results"
	"github.com/openshift/ci-tools/pkg/steps/utils"
)

const (
	// MultiStageTestLabel is the label we use to mark a pod as part of a multi-stage test
	MultiStageTestLabel = "ci.openshift.io/multi-stage-test"
	// ClusterProfileMountPath is where we mount the cluster profile in a pod
	ClusterProfileMountPath = "/var/run/secrets/ci.openshift.io/cluster-profile"
	// SecretMountPath is where we mount the shared dir secret
	SecretMountPath = "/var/run/secrets/ci.openshift.io/multi-stage"
	// SecretMountEnv is the env we use to expose the shared dir
	SecretMountEnv = "SHARED_DIR"
	// ClusterProfileMountEnv is the env we use to expose the cluster profile dir
	ClusterProfileMountEnv = "CLUSTER_PROFILE_DIR"
	// CliMountPath is where we mount the cli in a pod
	CliMountPath = "/cli"
	// CliEnv if the env we use to expose the path to the cli
	CliEnv = "CLI_DIR"
	// CommandPrefix is the prefix we add to a user's commands
	CommandPrefix = "#!/bin/bash\n" +
		"set -eu\n" +
		// provide a writeable kubeconfig so ppl can for example set a namespace, but do not pass it on to subsequent steps to limit the amount of possible footshooting
		"if [[ -e ${KUBECONFIG:-} ]]; then WRITEABLE_KUBECONFIG_LOCATION=$(mktemp) && cp $KUBECONFIG $WRITEABLE_KUBECONFIG_LOCATION && export KUBECONFIG=$WRITEABLE_KUBECONFIG_LOCATION && unset WRITEABLE_KUBECONFIG_LOCATION; fi\n" +
		// provide a home so kubectl discovery can be cached
		"if ! [[ -d ${HOME:-} ]]; then export HOME=/alabama; fi\n"
)

var envForProfile = []string{
	utils.ReleaseImageEnv(api.LatestReleaseName),
	DefaultLeaseEnv,
	utils.ImageFormatEnv,
}

type multiStageTestStep struct {
	name    string
	profile api.ClusterProfile
	config  *api.ReleaseBuildConfiguration
	// params exposes getters for variables created by other steps
	params             api.Parameters
	env                api.TestEnvironment
	client             PodClient
	artifactDir        string
	jobSpec            *api.JobSpec
	pre, test, post    []api.LiteralTestStep
	subTests           []*junit.TestCase
	allowSkipOnSuccess *bool
	leases             []api.StepLease
}

func MultiStageTestStep(
	testConfig api.TestStepConfiguration,
	config *api.ReleaseBuildConfiguration,
	params api.Parameters,
	client PodClient,
	artifactDir string,
	jobSpec *api.JobSpec,
	leases []api.StepLease,
) api.Step {
	return newMultiStageTestStep(testConfig, config, params, client, artifactDir, jobSpec, leases)
}

func newMultiStageTestStep(
	testConfig api.TestStepConfiguration,
	config *api.ReleaseBuildConfiguration,
	params api.Parameters,
	client PodClient,
	artifactDir string,
	jobSpec *api.JobSpec,
	leases []api.StepLease,
) *multiStageTestStep {
	if artifactDir != "" {
		artifactDir = filepath.Join(artifactDir, testConfig.As)
	}
	ms := testConfig.MultiStageTestConfigurationLiteral
	return &multiStageTestStep{
		name:               testConfig.As,
		profile:            ms.ClusterProfile,
		config:             config,
		params:             params,
		env:                ms.Environment,
		client:             client,
		artifactDir:        artifactDir,
		jobSpec:            jobSpec,
		pre:                ms.Pre,
		test:               ms.Test,
		post:               ms.Post,
		allowSkipOnSuccess: ms.AllowSkipOnSuccess,
		leases:             leases,
	}
}

func (s *multiStageTestStep) profileSecretName() string {
	return s.name + "-cluster-profile"
}

func (s *multiStageTestStep) Inputs() (api.InputDefinition, error) {
	return nil, nil
}

func (*multiStageTestStep) Validate() error { return nil }

func (s *multiStageTestStep) Run(ctx context.Context) error {
	return results.ForReason("executing_multi_stage_test").ForError(s.run(ctx))
}

func (s *multiStageTestStep) run(ctx context.Context) error {
	env, err := s.environment(ctx)
	if err != nil {
		return err
	}
	if err := s.createSecret(ctx); err != nil {
		return fmt.Errorf("failed to create secret: %w", err)
	}
	if err := s.createCredentials(); err != nil {
		return fmt.Errorf("failed to create credentials: %w", err)
	}
	if err := s.setupRBAC(); err != nil {
		return fmt.Errorf("failed to create RBAC objects: %w", err)
	}
	var errs []error
	if err := s.runSteps(ctx, s.pre, env, true, false); err != nil {
		errs = append(errs, fmt.Errorf("%q pre steps failed: %w", s.name, err))
	} else if err := s.runSteps(ctx, s.test, env, true, len(errs) != 0); err != nil {
		errs = append(errs, fmt.Errorf("%q test steps failed: %w", s.name, err))
	}
	if err := s.runSteps(context.Background(), s.post, env, false, len(errs) != 0); err != nil {
		errs = append(errs, fmt.Errorf("%q post steps failed: %w", s.name, err))
	}
	return utilerrors.NewAggregate(errs)
}

func (s *multiStageTestStep) Name() string { return s.name }
func (s *multiStageTestStep) Description() string {
	return fmt.Sprintf("Run multi-stage test %s", s.name)
}
func (s *multiStageTestStep) Objects() []ctrlruntimeclient.Object {
	return s.client.Objects()
}

func (s *multiStageTestStep) Requires() (ret []api.StepLink) {
	var needsReleaseImage, needsReleasePayload bool
	internalLinks := map[api.PipelineImageStreamTagReference]struct{}{}
	for _, step := range append(append(s.pre, s.test...), s.post...) {
		if s.config.IsPipelineImage(step.From) || s.config.BuildsImage(step.From) {
			internalLinks[api.PipelineImageStreamTagReference(step.From)] = struct{}{}
		} else {
			needsReleaseImage = true
		}

		if link, ok := step.FromImageTag(); ok {
			internalLinks[link] = struct{}{}
		}

		for _, dependency := range step.Dependencies {
			// we validate that the link will exist at config load time
			// so we can safely ignore the case where !ok
			imageStream, name, _ := s.config.DependencyParts(dependency)
			ret = append(ret, api.LinkForImage(imageStream, name))
		}

		if step.Cli != "" {
			dependency := api.StepDependency{Name: fmt.Sprintf("%s:cli", api.ReleaseStreamFor(step.Cli))}
			imageStream, name, _ := s.config.DependencyParts(dependency)
			ret = append(ret, api.LinkForImage(imageStream, name))
		}
	}
	for link := range internalLinks {
		ret = append(ret, api.InternalImageLink(link))
	}
	if s.profile != "" {
		needsReleasePayload = true
		for _, env := range envForProfile {
			if link, ok := utils.LinkForEnv(env); ok {
				ret = append(ret, link)
			}
		}
	}
	if needsReleaseImage && !needsReleasePayload {
		ret = append(ret, api.ReleaseImagesLink(api.LatestReleaseName))
	}
	return
}

func (s *multiStageTestStep) Creates() []api.StepLink { return nil }
func (s *multiStageTestStep) Provides() api.ParameterMap {
	return nil
}
func (s *multiStageTestStep) SubTests() []*junit.TestCase { return s.subTests }

func (s *multiStageTestStep) setupRBAC() error {
	labels := map[string]string{MultiStageTestLabel: s.name}
	m := meta.ObjectMeta{Namespace: s.jobSpec.Namespace(), Name: s.name, Labels: labels}
	sa := &coreapi.ServiceAccount{ObjectMeta: m}
	role := &rbacapi.Role{
		ObjectMeta: m,
		Rules: []rbacapi.PolicyRule{{
			APIGroups: []string{"rbac.authorization.k8s.io"},
			Resources: []string{"rolebindings"},
			Verbs:     []string{"create", "list"},
		}, {
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{s.name},
			Verbs:         []string{"get", "update"},
		}, {
			APIGroups: []string{"", "image.openshift.io"},
			Resources: []string{"imagestreams/layers"},
			Verbs:     []string{"get"},
		}},
	}
	subj := []rbacapi.Subject{{Kind: "ServiceAccount", Name: s.name}}
	binding := &rbacapi.RoleBinding{
		ObjectMeta: m,
		RoleRef:    rbacapi.RoleRef{Kind: "Role", Name: s.name},
		Subjects:   subj,
	}
	check := func(err error) bool {
		return err == nil || kerrors.IsAlreadyExists(err)
	}
	if err := s.client.Create(context.TODO(), sa); !check(err) {
		return err
	}
	if err := s.client.Create(context.TODO(), role); !check(err) {
		return err
	}
	if err := s.client.Create(context.TODO(), binding); !check(err) {
		return err
	}
	return nil
}

func (s *multiStageTestStep) environment(ctx context.Context) ([]coreapi.EnvVar, error) {
	var ret []coreapi.EnvVar
	for _, l := range s.leases {
		val, err := s.params.Get(l.Env)
		if err != nil {
			return nil, err
		}
		ret = append(ret, coreapi.EnvVar{Name: l.Env, Value: val})
	}
	if s.profile != "" {
		secret := s.profileSecretName()
		if err := s.client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: s.jobSpec.Namespace(), Name: secret}, &coreapi.Secret{}); err != nil {
			return nil, fmt.Errorf("could not find secret %q: %w", secret, err)
		}
		for _, e := range envForProfile {
			val, err := s.params.Get(e)
			if err != nil {
				return nil, err
			}
			ret = append(ret, coreapi.EnvVar{Name: e, Value: val})
		}
		if optionalOperator, err := resolveOptionalOperator(s.params); err != nil {
			return nil, err
		} else if optionalOperator != nil {
			ret = append(ret, optionalOperator.asEnv()...)
		}
	}
	return ret, nil
}

func (s *multiStageTestStep) createSecret(ctx context.Context) error {
	log.Printf("Creating multi-stage test secret %q", s.name)
	secret := &coreapi.Secret{ObjectMeta: meta.ObjectMeta{Namespace: s.jobSpec.Namespace(), Name: s.name}}
	if err := s.client.Delete(ctx, secret); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("cannot delete secret %q: %w", s.name, err)
	}
	return s.client.Create(ctx, secret)
}

func (s *multiStageTestStep) createCredentials() error {
	log.Printf("Creating multi-stage test credentials for %q", s.name)
	toCreate := map[string]*coreapi.Secret{}
	for _, step := range append(s.pre, append(s.test, s.post...)...) {
		for _, credential := range step.Credentials {
			// we don't want secrets imported from separate namespaces to collide
			// but we want to keep them generally recognizable for debugging, and the
			// chance we get a second-level collision (ns-a, name) and (ns, a-name) is
			// small, so we can get away with this string prefixing
			name := fmt.Sprintf("%s-%s", credential.Namespace, credential.Name)
			raw := &coreapi.Secret{}
			if err := s.client.Get(context.TODO(), ctrlruntimeclient.ObjectKey{Namespace: credential.Namespace, Name: credential.Name}, raw); err != nil {
				return fmt.Errorf("could not read source credential: %w", err)
			}
			toCreate[name] = &coreapi.Secret{
				TypeMeta: raw.TypeMeta,
				ObjectMeta: meta.ObjectMeta{
					Name:      name,
					Namespace: s.jobSpec.Namespace(),
				},
				Type:       raw.Type,
				Data:       raw.Data,
				StringData: raw.StringData,
			}
		}
	}

	for name := range toCreate {
		if err := s.client.Create(context.TODO(), toCreate[name]); err != nil && !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("could not create source credential: %w", err)
		}
	}
	return nil
}

func (s *multiStageTestStep) runSteps(
	ctx context.Context,
	steps []api.LiteralTestStep,
	env []coreapi.EnvVar,
	shortCircuit bool,
	hasPrevErrs bool,
) error {
	pods, err := s.generatePods(steps, env, hasPrevErrs)
	if err != nil {
		return err
	}
	var errs []error
	if err := s.runPods(ctx, pods, shortCircuit); err != nil {
		errs = append(errs, err)
	}
	select {
	case <-ctx.Done():
		log.Printf("cleanup: Deleting pods with label %s=%s", MultiStageTestLabel, s.name)
		if err := s.client.DeleteAllOf(cleanupCtx, &coreapi.Pod{}, ctrlruntimeclient.InNamespace(s.jobSpec.Namespace()), ctrlruntimeclient.MatchingLabels{MultiStageTestLabel: s.name}); err != nil && !kerrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to delete pods with label %s=%s: %w", MultiStageTestLabel, s.name, err))
		}
		errs = append(errs, fmt.Errorf("cancelled"))
	default:
		break
	}
	return utilerrors.NewAggregate(errs)
}

const multiStageTestStepContainerName = "test"

func (s *multiStageTestStep) generatePods(steps []api.LiteralTestStep, env []coreapi.EnvVar,
	hasPrevErrs bool) ([]coreapi.Pod, error) {
	var ret []coreapi.Pod
	var errs []error
	for _, step := range steps {
		if s.allowSkipOnSuccess != nil && *s.allowSkipOnSuccess &&
			step.OptionalOnSuccess != nil && *step.OptionalOnSuccess &&
			!hasPrevErrs {
			continue
		}
		image := step.From
		if link, ok := step.FromImageTag(); ok {
			image = fmt.Sprintf("%s:%s", api.PipelineImageStream, link)
		} else {
			dep := api.StepDependency{Name: image}
			stream, tag, _ := s.config.DependencyParts(dep)
			image = fmt.Sprintf("%s:%s", stream, tag)
		}
		resources, err := resourcesFor(step.Resources)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		name := fmt.Sprintf("%s-%s", s.name, step.As)
		pod, err := generateBasePod(s.jobSpec, name, multiStageTestStepContainerName, []string{"/bin/bash", "-c", CommandPrefix + step.Commands}, image, resources, step.ArtifactDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		delete(pod.Labels, ProwJobIdLabel)
		pod.Annotations[annotationSaveContainerLogs] = "true"
		pod.Labels[MultiStageTestLabel] = s.name
		pod.Spec.ActiveDeadlineSeconds = step.ActiveDeadlineSeconds
		pod.Spec.ServiceAccountName = s.name
		pod.Spec.TerminationGracePeriodSeconds = step.TerminationGracePeriodSeconds
		pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{Name: homeVolumeName, VolumeSource: coreapi.VolumeSource{EmptyDir: &coreapi.EmptyDirVolumeSource{}}})
		for idx := range pod.Spec.Containers {
			if pod.Spec.Containers[idx].Name != multiStageTestStepContainerName {
				continue
			}
			pod.Spec.Containers[idx].VolumeMounts = append(pod.Spec.Containers[idx].VolumeMounts, coreapi.VolumeMount{Name: homeVolumeName, MountPath: "/alabama"})
		}

		if !step.ReadonlySharedDir {
			addSecretWrapper(pod)
		}
		container := &pod.Spec.Containers[0]
		container.Env = append(container.Env, []coreapi.EnvVar{
			{Name: "NAMESPACE", Value: s.jobSpec.Namespace()},
			{Name: "JOB_NAME_SAFE", Value: strings.Replace(s.name, "_", "-", -1)},
			{Name: "JOB_NAME_HASH", Value: s.jobSpec.JobNameHash()},
		}...)
		container.Env = append(container.Env, env...)
		container.Env = append(container.Env, s.generateParams(step.Environment)...)
		depEnv, depErrs := s.envForDependencies(step)
		if len(depErrs) != 0 {
			errs = append(errs, depErrs...)
			continue
		}
		container.Env = append(container.Env, depEnv...)
		if owner := s.jobSpec.Owner(); owner != nil {
			pod.OwnerReferences = append(pod.OwnerReferences, *owner)
		}
		if s.profile != "" {
			addProfile(s.profileSecretName(), s.profile, pod)
			container.Env = append(container.Env, []coreapi.EnvVar{
				{Name: "KUBECONFIG", Value: filepath.Join(SecretMountPath, "kubeconfig")},
				{Name: "KUBEADMIN_PASSWORD_FILE", Value: filepath.Join(SecretMountPath, "kubeadmin-password")},
			}...)
		}
		if step.Cli != "" {
			addCliInjector(step.Cli, pod)
		}
		addSecret(s.name, pod)
		addCredentials(step.Credentials, pod)
		ret = append(ret, *pod)
	}
	return ret, utilerrors.NewAggregate(errs)
}

func (s *multiStageTestStep) envForDependencies(step api.LiteralTestStep) ([]coreapi.EnvVar, []error) {
	var env []coreapi.EnvVar
	var errs []error
	for _, dependency := range step.Dependencies {
		imageStream, name, _ := s.config.DependencyParts(dependency)
		ref, err := utils.ImageDigestFor(s.client, s.jobSpec.Namespace, imageStream, name)()
		if err != nil {
			errs = append(errs, fmt.Errorf("could not determine image pull spec for image %s on step %s", dependency.Name, step.As))
			continue
		}
		env = append(env, coreapi.EnvVar{
			Name: dependency.Env, Value: ref,
		})
	}
	return env, errs
}

func addSecretWrapper(pod *coreapi.Pod) {
	volume := "secret-wrapper"
	dir := "/tmp/secret-wrapper"
	bin := filepath.Join(dir, "secret-wrapper")
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{
		Name: volume,
		VolumeSource: coreapi.VolumeSource{
			EmptyDir: &coreapi.EmptyDirVolumeSource{},
		},
	})
	mount := coreapi.VolumeMount{Name: volume, MountPath: dir}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, coreapi.Container{
		Image:                    fmt.Sprintf("%s/ci/secret-wrapper:latest", apiCIRegistry),
		Name:                     "cp-secret-wrapper",
		Command:                  []string{"cp"},
		Args:                     []string{"/bin/secret-wrapper", bin},
		VolumeMounts:             []coreapi.VolumeMount{mount},
		TerminationMessagePolicy: coreapi.TerminationMessageFallbackToLogsOnError,
	})
	container := &pod.Spec.Containers[0]
	container.Args = append([]string{}, append(container.Command, container.Args...)...)
	container.Command = []string{bin}
	container.VolumeMounts = append(container.VolumeMounts, mount)
}

func (s *multiStageTestStep) generateParams(env []api.StepParameter) []coreapi.EnvVar {
	var ret []coreapi.EnvVar
	for _, env := range env {
		value := ""
		if env.Default != nil {
			value = *env.Default
		}
		if v, ok := s.env[env.Name]; ok {
			value = v
		}
		ret = append(ret, coreapi.EnvVar{Name: env.Name, Value: value})
	}
	return ret
}

func addSecret(secret string, pod *coreapi.Pod) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{
		Name: secret,
		VolumeSource: coreapi.VolumeSource{
			Secret: &coreapi.SecretVolumeSource{SecretName: secret},
		},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, coreapi.VolumeMount{
		Name:      secret,
		MountPath: SecretMountPath,
	})
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, coreapi.EnvVar{
		Name:  SecretMountEnv,
		Value: SecretMountPath,
	})
}

func addCredentials(credentials []api.CredentialReference, pod *coreapi.Pod) {
	for _, credential := range credentials {
		name := fmt.Sprintf("%s-%s", credential.Namespace, credential.Name)
		pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{
			Name: name,
			VolumeSource: coreapi.VolumeSource{
				Secret: &coreapi.SecretVolumeSource{SecretName: name},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, coreapi.VolumeMount{
			Name:      name,
			MountPath: credential.MountPath,
		})
	}
}

func addProfile(name string, profile api.ClusterProfile, pod *coreapi.Pod) {
	volumeName := "cluster-profile"
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{
		Name: volumeName,
		VolumeSource: coreapi.VolumeSource{
			Secret: &coreapi.SecretVolumeSource{
				SecretName: name,
			},
		},
	})
	container := &pod.Spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, coreapi.VolumeMount{
		Name:      volumeName,
		MountPath: ClusterProfileMountPath,
	})
	container.Env = append(container.Env, []coreapi.EnvVar{{
		Name:  "CLUSTER_TYPE",
		Value: profile.ClusterType(),
	}, {
		Name:  ClusterProfileMountEnv,
		Value: ClusterProfileMountPath,
	}}...)
}

func addCliInjector(release string, pod *coreapi.Pod) {
	volumeName := "cli"
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreapi.Volume{
		Name: volumeName,
		VolumeSource: coreapi.VolumeSource{
			EmptyDir: &coreapi.EmptyDirVolumeSource{},
		},
	})
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, coreapi.Container{
		Name:    "inject-cli",
		Image:   fmt.Sprintf("%s:cli", api.ReleaseStreamFor(release)),
		Command: []string{"/bin/cp"},
		Args:    []string{"/usr/bin/oc", CliMountPath},
		VolumeMounts: []coreapi.VolumeMount{{
			Name:      volumeName,
			MountPath: CliMountPath,
		}},
	})
	container := &pod.Spec.Containers[0]
	var args []string
	for _, arg := range container.Args {
		if strings.HasPrefix(arg, CommandPrefix) {
			args = append(args, fmt.Sprintf("%s%s\n%s", CommandPrefix, `export PATH="${PATH}:${CLI_DIR}"`, strings.TrimPrefix(arg, CommandPrefix)))
		} else {
			args = append(args, arg)
		}
	}
	container.Args = args
	container.VolumeMounts = append(container.VolumeMounts, coreapi.VolumeMount{
		Name:      volumeName,
		MountPath: CliMountPath,
	})
	container.Env = append(container.Env, coreapi.EnvVar{
		Name:  CliEnv,
		Value: CliMountPath,
	})
}

func (s *multiStageTestStep) runPods(ctx context.Context, pods []coreapi.Pod, shortCircuit bool) error {
	namePrefix := s.name + "-"
	var errs []error
	for _, pod := range pods {
		var notifier ContainerNotifier = NopNotifier
		for _, c := range pod.Spec.Containers {
			if c.Name == "artifacts" {
				container := pod.Spec.Containers[0].Name
				dir := filepath.Join(s.artifactDir, strings.TrimPrefix(pod.Name, namePrefix))
				artifacts := NewArtifactWorker(s.client, dir, s.jobSpec.Namespace())
				artifacts.CollectFromPod(pod.Name, []string{container}, nil)
				notifier = artifacts
				break
			}
		}
		err := s.runPod(ctx, &pod, NewTestCaseNotifier(notifier))
		if err != nil {
			errs = append(errs, err)
			if shortCircuit {
				break
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func (s *multiStageTestStep) runPod(ctx context.Context, pod *coreapi.Pod, notifier *TestCaseNotifier) error {
	if _, err := createOrRestartPod(s.client, pod); err != nil {
		return fmt.Errorf("failed to create or restart %q pod: %w", pod.Name, err)
	}
	newPod, err := waitForPodCompletion(ctx, s.client, pod.Namespace, pod.Name, notifier, false)
	if newPod != nil {
		pod = newPod
	}
	s.subTests = append(s.subTests, notifier.SubTests(fmt.Sprintf("%s - %s ", s.Description(), pod.Name))...)
	if err != nil {
		linksText := strings.Builder{}
		linksText.WriteString(fmt.Sprintf("Link to step on registry info site: https://steps.ci.openshift.org/reference/%s", strings.TrimPrefix(pod.Name, s.name+"-")))
		linksText.WriteString(fmt.Sprintf("\nLink to job on registry info site: https://steps.ci.openshift.org/job?org=%s&repo=%s&branch=%s&test=%s", s.config.Metadata.Org, s.config.Metadata.Repo, s.config.Metadata.Branch, s.name))
		if s.config.Metadata.Variant != "" {
			linksText.WriteString(fmt.Sprintf("&variant=%s", s.config.Metadata.Variant))
		}
		status := "failed"
		if pod.Status.Phase == coreapi.PodFailed && pod.Status.Reason == "DeadlineExceeded" {
			status = "exceeded the configured timeout"
			if pod.Spec.ActiveDeadlineSeconds != nil {
				status = fmt.Sprintf("%s activeDeadlineSeconds=%d", status, *pod.Spec.ActiveDeadlineSeconds)
			}
		}
		return fmt.Errorf("%q pod %q %s: %w\n%s", s.name, pod.Name, status, err, linksText.String())
	}
	return nil
}
