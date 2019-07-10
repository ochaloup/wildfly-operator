package util

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

//ExecRemote executes a command inside the remote pod
func ExecRemote(pod corev1.Pod, command string) (string, error) {
	fmt.Println(">>>>>>>>>>>>>>> at start...")
	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)
	fmt.Println(">>>> kubeconfig taking")
	// Instantiate loader for kubeconfig file.
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	// Determine the Namespace referenced by the current context in the
	// kubeconfig file.
	/*
		namespace, _, err := kubeconfig.Namespace()
		if err != nil {
			panic(err)
		}
	*/
	fmt.Println(">>>> kubeconfig taken", kubeconfig)
	// Get a rest.Config from the kubeconfig file.  This will be passed into all
	// the client objects we create.
	restconfig, err := kubeconfig.ClientConfig()
	if err != nil {
		return "", err
	}

	// Create a Kubernetes core/v1 client.
	coreclient, err := corev1client.NewForConfig(restconfig)
	if err != nil {
		return "", err
	}
	fmt.Println(">>>> preparing rest clinet")
	// Prepare the API URL used to execute another process within the Pod.  In
	// this case, we'll run a remote shell.
	req := coreclient.RESTClient().
		Post().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: pod.Spec.Containers[0].Name,
			Command:   []string{"/bin/sh", "-c", command},
			// Stdin:     true,
			Stdout: true,
			Stderr: true,
			// TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restconfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	fmt.Println(">>>> going to stream")
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &execOut,
		Stderr: &execErr,
		Tty:    false,
	})

	if err != nil {
		return "", fmt.Errorf("cannot execute '%v': %v", command, err)
	}
	if execErr.Len() > 0 {
		return "", fmt.Errorf("command execution '%v' got stderr: %v", command, execErr.String())
	}
	result := execOut.String()
	fmt.Println(">>>> going to read exec out", result)
	return result, nil
}

func newStringReader(ss []string) io.Reader {
	formattedString := strings.Join(ss, "\n")
	reader := strings.NewReader(formattedString)
	return reader
}

type writer struct {
	Str []string
}

func (w *writer) Write(p []byte) (n int, err error) {
	str := string(p)
	if len(str) > 0 {
		w.Str = append(w.Str, str)
	}
	return len(str), nil
}
