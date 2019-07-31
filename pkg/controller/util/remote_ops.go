package util

import (
	"bufio"
	"bytes"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

//ExecRemote executes a command inside the remote pod
func ExecRemote(pod corev1.Pod, command string) (string, error) {
	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	// Instantiate loader for kubeconfig file.
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

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
	return execOut.String(), nil
}

// SocketConnect send a command (a string) to the defined hostname and port
//  where it connects to with 'net.Dial' tcp connection
func SocketConnect(hostname string, port int32, command string) (string, error) {
	// connect to socket
	toConnectTo := fmt.Sprintf("%v:%v", hostname, port)
	conn, _ := net.Dial("tcp", toConnectTo)
	// send to socket
	fmt.Fprintf(conn, command+"\n")
	// blocking operation, listen for reply
	message, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("Error to get response for command %s sending to %s:%v, error: %v",
			command, hostname, port, err)
	}
	return message, nil
}
