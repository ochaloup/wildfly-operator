package util

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	numberOne = int64(1)
	// options to tail one line from the log of a pod
	tailOneLineLogOptions = v1.PodLogOptions{TailLines: &numberOne, Timestamps: true}
	// regexp searching for timestamp at the start of the string
	timestampFromLogLine = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[^ ]+`)
)

//ExecRemote executes a command inside the remote pod
func ExecRemote(pod *corev1.Pod, command string) (string, error) {
	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	// Create a Kubernetes core/v1 client.
	restconfig, err := getKubeRestConfig()
	if err != nil {
		return "", err
	}
	coreclient, err := getKubeCoreClient(restconfig)
	if err != nil {
		return "", err
	}

	// Prepare the API URL used to execute another process within the Pod.  In
	// this case, we'll run a remote shell.
	req := coreclient.RESTClient().
		Post().
		Namespace(pod.Namespace).
		Name(pod.Name).
		Resource("pods").
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
	conn, err := net.Dial("tcp", toConnectTo)
	if err != nil {
		return "", fmt.Errorf("Cannot process TCP connection to %v, error: %v",
			toConnectTo, err)
	}
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

// ReadPodLog reads log from the specified pod. Using Kubernetes REST Client to query the API
func ReadPodLog(pod *corev1.Pod, logOptions *corev1.PodLogOptions) (io.ReadCloser, error) {
	// Create a Kubernetes core/v1 client.
	restconfig, err := getKubeRestConfig()
	if err != nil {
		return nil, err
	}
	coreclient, err := getKubeCoreClient(restconfig)
	if err != nil {
		return nil, err
	}

	req := coreclient.RESTClient().
		Get().
		Namespace(pod.Namespace).
		Name(pod.Name).
		Resource("pods").
		SubResource("log")

	if logOptions.Container != "" { // there could be multiple container in the pod
		req.Param("container", logOptions.Container)
	}
	if logOptions.Follow { // follow the log stream
		req.Param("follow", strconv.FormatBool(logOptions.Follow))
	}
	if logOptions.Previous { // log from previously terminated pod
		req.Param("previous", strconv.FormatBool(logOptions.Previous))
	}
	if logOptions.Timestamps { // timestamp at the beginning of every line
		req.Param("timestamps", strconv.FormatBool(logOptions.Timestamps))
	}
	if logOptions.SinceSeconds != nil { // log records newer of since seconds are printed
		req.Param("sinceSeconds", strconv.FormatInt(*logOptions.SinceSeconds, 10))
	}
	if logOptions.SinceTime != nil { // an RFC3339 timestamp from which to show logs
		req.Param("sinceTime", logOptions.SinceTime.Format(time.RFC3339))
	}
	if logOptions.TailLines != nil { // print defined number of lines from the end
		req.Param("tailLines", strconv.FormatInt(*logOptions.TailLines, 10))
	}
	if logOptions.LimitBytes != nil { // number of bytes to read from the server before terminating output
		req.Param("limitBytes", strconv.FormatInt(*logOptions.LimitBytes, 10))
	}

	return req.Stream() // execution of the request
}

// ObtainLogLatestTimestamp reads log from pod and find out
//   what is the most latest log record at the time
//   and returns time stamp of the record
func ObtainLogLatestTimestamp(pod *corev1.Pod) (*time.Time, error) {
	lineReader, err := ReadPodLog(pod, &tailOneLineLogOptions)
	if err != nil {
		return nil, fmt.Errorf("Cannot read log from pod %v, error: %v", pod.Name, err)
	}
	line, err := readerToString(lineReader)
	if err != nil {
		return nil, fmt.Errorf("Cannot read string from the reader log from pod %v, error: %v", pod.Name, err)
	}
	parsedTimestamp := timestampFromLogLine.FindString(line)
	if parsedTimestamp == "" {
		return nil, fmt.Errorf("It was not succesful to parse the log line '%s' to get time stamp "+
			"expected to be placed at the start of the string", line)
	}
	parsedTime, err := time.Parse(time.RFC3339, parsedTimestamp)
	if err != nil {
		parsedTime, err = time.Parse(time.RFC3339Nano, parsedTimestamp)
		if err != nil {
			return nil, fmt.Errorf("Error on converting time stamp '%v' in string format "+
				"to golang type Time, error: %v", parsedTimestamp, err)
		}
	}
	return &parsedTime, err
}

// VerifyLogContainsRegexp checks if a line in the log from the pod matches the provided regexp
//   the log could be limited to be taken from particular time further, when no time defined the log is not limited by time
func VerifyLogContainsRegexp(pod *corev1.Pod, logFromTime *time.Time, regexpLineCheck *regexp.Regexp) (string, error) {
	timeLimitingPodLogOptions := v1.PodLogOptions{}
	if logFromTime != nil {
		metav1LogFromTime := metav1.NewTime(*logFromTime)
		timeLimitingPodLogOptions.SinceTime = &metav1LogFromTime
	}
	lineReader, err := ReadPodLog(pod, &timeLimitingPodLogOptions)
	if err != nil {
		return "", fmt.Errorf("Cannot read log from pod %v, error: %v", pod.Name, err)
	}
	defer lineReader.Close()

	scanner := bufio.NewScanner(lineReader)
	for scanner.Scan() {
		line := scanner.Text()
		if regexpLineCheck.MatchString(line) {
			return line, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("Failed to finish reading log from pod name %v starting at time %v, error: %v", pod.Name, logFromTime, err)
	}
	return "", nil
}

func getKubeRestConfig() (*restclient.Config, error) {
	// Instantiate loader for kubeconfig file.
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	// Get a rest.Config from the kubeconfig file.  This will be passed into all
	// the client objects we create.
	restconfig, err := kubeconfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("Cannot create ClientConfig for kube config '%v', error: %v", kubeconfig, err)
	}
	return restconfig, nil
}

func getKubeCoreClient(rsconfig *restclient.Config) (*corev1client.CoreV1Client, error) {
	// Create a Kubernetes core/v1 client.
	coreclient, err := corev1client.NewForConfig(rsconfig)
	if err != nil {
		return nil, fmt.Errorf("Cannot get Kube core/v1 client for rest config '%v', error: %v", rsconfig, err)
	}
	return coreclient, nil
}

func readerToString(reader io.ReadCloser) (string, error) {
	defer reader.Close()

	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(reader)
	return buf.String(), err
}
