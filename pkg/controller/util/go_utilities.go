package util

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

var (
	regexpPatternEndsWithNumber = regexp.MustCompile(`[0-9]+$`)
)

// ContainsInMap returns true if the map m contains at least one of the string
//   presented as argument `vals`, otherwise false is returned
func ContainsInMap(m map[string]string, vals ...string) bool {
	for _, x := range m {
		for _, v := range vals {
			if x == v {
				return true
			}
		}
	}
	return false
}

// ContainsInList returns true if the string s is included in the list,
//   otherwise false is returned
func ContainsInList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RemoveFromList iterates over the list and removes all occurences
//   of the string s. The list without the s strings is returned.
func RemoveFromList(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			list = append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// SortPodListByName sorts the pod list by number in the name
//  expecting the format which the StatefulSet works with which is `<podname>-<number>`
func SortPodListByName(podList *corev1.PodList) *corev1.PodList {
	sort.SliceStable(podList.Items, func(i, j int) bool {
		reOut1 := regexpPatternEndsWithNumber.FindStringSubmatch(podList.Items[i].ObjectMeta.Name)
		if reOut1 == nil {
			return false
		}
		number1, err := strconv.Atoi(reOut1[0])
		if err != nil {
			return false
		}
		reOut2 := regexpPatternEndsWithNumber.FindStringSubmatch(podList.Items[j].ObjectMeta.Name)
		if reOut2 == nil {
			return false
		}
		number2, err := strconv.Atoi(reOut2[0])
		if err != nil {
			return false
		}

		return number1 < number2
	})
	return podList
}

// MapMerge merges the two maps together and returns the result.
// If one of them is nil then is ommitted from the merged result
// If both maps are null then an empty initialized map is returned
// The second map rewrites data of the first map in the result if there are such
func MapMerge(firstMap map[string]string, secondOverwritingMap map[string]string) map[string]string {
	returnedMap := make(map[string]string)
	for v, k := range firstMap {
		returnedMap[v] = k
	}
	for v, k := range secondOverwritingMap {
		returnedMap[v] = k
	}
	return returnedMap
}

// GetEnvAsInt returns defined environment variable as an integer
//  or default value is returned if the env var is not configured
func GetEnvAsInt(key string, fallbackInteger int64) int64 {
	valueStr, ok := os.LookupEnv(key)
	if ok {
		valueInt, err := strconv.ParseInt(valueStr, 10, 64)
		if err == nil {
			return valueInt
		}
	}
	return fallbackInteger
}

// GetEnvAsDurationInSeconds returns defined environment variable as duration
//  while it expects the environment variable contains the number of seconds
//  that will be converted to duration
//  or default value is returned if the env var is not configured
func GetEnvAsDurationInSeconds(key string, fallbackSeconds int64) time.Duration {
	valueSeconds := GetEnvAsInt(key, fallbackSeconds)
	return time.Duration(valueSeconds) * time.Second
}
