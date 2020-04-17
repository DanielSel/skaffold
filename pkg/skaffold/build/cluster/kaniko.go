/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
)

const initContainer = "kaniko-init-container"

func (b *Builder) buildWithKaniko(ctx context.Context, out io.Writer, workspace string, artifact *latest.KanikoArtifact, tag string) (string, error) {
	generatedEnvs, err := generateEnvFromImage(tag)
	if err != nil {
		return "", fmt.Errorf("error processing generated env variables from image uri: %w", err)
	}
	env, err := evaluateEnv(artifact.Env, generatedEnvs...)
	if err != nil {
		return "", fmt.Errorf("unable to evaluate env variables: %w", err)
	}
	artifact.Env = env

	client, err := kubernetes.Client()
	if err != nil {
		return "", fmt.Errorf("getting Kubernetes client: %w", err)
	}
	pods := client.CoreV1().Pods(b.Namespace)

	podSpec, err := b.kanikoPodSpec(artifact, tag)
	if err != nil {
		return "", err
	}

	pod, err := pods.Create(podSpec)
	if err != nil {
		return "", fmt.Errorf("creating kaniko pod: %w", err)
	}
	defer func() {
		if err := pods.Delete(pod.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: new(int64),
		}); err != nil {
			logrus.Fatalf("deleting pod: %s", err)
		}
	}()

	if err := b.copyKanikoBuildContext(ctx, workspace, artifact, pods, pod.Name); err != nil {
		return "", fmt.Errorf("copying sources: %w", err)
	}

	// Wait for the pods to succeed while streaming the logs
	waitForLogs := streamLogs(ctx, out, pod.Name, pods)

	if err := kubernetes.WaitForPodSucceeded(ctx, pods, pod.Name, b.timeout); err != nil {
		waitForLogs()
		return "", err
	}

	waitForLogs()

	return docker.RemoteDigest(tag, b.insecureRegistries)
}

// first copy over the buildcontext tarball into the init container tmp dir via kubectl cp
// Via kubectl exec, we extract the tarball to the empty dir
// Then, via kubectl exec, create the /tmp/complete file via kubectl exec to complete the init container
func (b *Builder) copyKanikoBuildContext(ctx context.Context, workspace string, artifact *latest.KanikoArtifact, pods corev1.PodInterface, podName string) error {
	if err := kubernetes.WaitForPodInitialized(ctx, pods, podName); err != nil {
		return fmt.Errorf("waiting for pod to initialize: %w", err)
	}

	buildCtx, buildCtxWriter := io.Pipe()
	go func() {
		err := docker.CreateDockerTarContext(ctx, buildCtxWriter, workspace, &latest.DockerArtifact{
			BuildArgs:      artifact.BuildArgs,
			DockerfilePath: artifact.DockerfilePath,
		}, b.insecureRegistries)
		if err != nil {
			buildCtxWriter.CloseWithError(fmt.Errorf("creating docker context: %w", err))
			return
		}
		buildCtxWriter.Close()
	}()

	// Send context by piping into `tar`.
	// In case of an error, print the command's output. (The `err` itself is useless: exit status 1).
	var out bytes.Buffer
	if err := b.kubectlcli.Run(ctx, buildCtx, &out, "exec", "-i", podName, "-c", initContainer, "-n", b.Namespace, "--", "tar", "-xf", "-", "-C", constants.DefaultKanikoEmptyDirMountPath); err != nil {
		return fmt.Errorf("uploading build context: %s", out.String())
	}

	// Generate a file to successfully terminate the init container.
	if out, err := b.kubectlcli.RunOut(ctx, "exec", podName, "-c", initContainer, "-n", b.Namespace, "--", "touch", "/tmp/complete"); err != nil {
		return fmt.Errorf("finishing upload of the build context: %s", out)
	}

	return nil
}

func evaluateEnv(env []v1.EnvVar, additional ...v1.EnvVar) ([]v1.EnvVar, error) {
	// Prepare additional envs
	addEnv := make(map[string]string)
	for _, addEnvVar := range additional {
		addEnv[addEnvVar.Name] = addEnvVar.Value
	}

	// Evaluate provided env variables
	var evaluated []v1.EnvVar
	for _, envVar := range env {
		val, err := util.ExpandEnvTemplate(envVar.Value, nil)
		if err != nil {
			return nil, fmt.Errorf("unable to get value for env variable %q: %w", envVar.Name, err)
		}

		evaluated = append(evaluated, v1.EnvVar{Name: envVar.Name, Value: val})

		// Provided env variables have higher priority than additional (generated) ones
		delete(addEnv, envVar.Name)
	}

	// Append additional (generated) env variables
	for name, value := range addEnv {
		if value != "" {
			evaluated = append(evaluated, v1.EnvVar{Name: name, Value: value})
		}
	}

	return evaluated, nil
}

func envMapFromVars(env []v1.EnvVar) map[string]string {
	envMap := make(map[string]string)
	for _, envVar := range env {
		envMap[envVar.Name] = envVar.Value
	}
	return envMap
}

func generateEnvFromImage(imageStr string) ([]v1.EnvVar, error) {
	repoStr, nameStr, tagStr, err := parseImageParts(imageStr)
	if err != nil {
		return nil, err
	}
	var generatedEnvs []v1.EnvVar
	generatedEnvs = append(generatedEnvs, v1.EnvVar{Name: "IMAGE_REPO", Value: repoStr})
	generatedEnvs = append(generatedEnvs, v1.EnvVar{Name: "IMAGE_NAME", Value: nameStr})
	generatedEnvs = append(generatedEnvs, v1.EnvVar{Name: "IMAGE_TAG", Value: tagStr})
	return generatedEnvs, nil
}

func parseImageParts(imageStr string) (string, string, string, error) {
	var repo, name, tag string
	var err error
	parts := strings.Split(imageStr, ":")
	switch len(parts) {
	case 1:
		// default tag: latest
		parts = append(parts, "latest")
	case 2:
	case 3:
		if strings.ContainsRune(parts[0], '/') {
			err = fmt.Errorf("invalid image uri string: %q", imageStr)
			return repo, name, tag, err
		}
		parts[0] = parts[0] + ":" + parts[1]
		parts[1] = parts[2]
		parts = parts[:2]
	default:
		err = fmt.Errorf("invalid image uri string: %q", imageStr)
		return repo, name, tag, err
	}
	tag = parts[1]
	imageParts := strings.Split(parts[0], "/")
	switch len(imageParts) {
	case 0:
		name = parts[1]
	case 1:
		name = imageParts[0]
	default:
		repo = strings.Join(imageParts[:len(imageParts)-1], "/")
		name = imageParts[len(imageParts)-1]
	}
	return repo, name, tag, err
}
