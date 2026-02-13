/*
Copyright 2026 The Kubernetes Authors.

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

package slos

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGatherScheduleTimes(t *testing.T) {
	testCases := []struct {
		name             string
		mapEventsByOrder bool
		podCreates       []struct {
			name string
			time time.Time
		}
		events []struct {
			podName string
			time    time.Time
		}
		wantScheduleTimes map[string]time.Time
		wantErr           bool
	}{
		{
			name:             "map by key (default)",
			mapEventsByOrder: false,
			podCreates: []struct {
				name string
				time time.Time
			}{
				{name: "pod1", time: time.Unix(100, 0)},
				{name: "pod2", time: time.Unix(200, 0)},
			},
			events: []struct {
				podName string
				time    time.Time
			}{
				{podName: "pod2", time: time.Unix(210, 0)},
				{podName: "pod1", time: time.Unix(110, 0)},
			},
			wantScheduleTimes: map[string]time.Time{
				"default/pod1": time.Unix(110, 0),
				"default/pod2": time.Unix(210, 0),
			},
		},
		{
			name:             "map by order",
			mapEventsByOrder: true,
			podCreates: []struct {
				name string
				time time.Time
			}{
				{name: "pod-late-create", time: time.Unix(200, 0)},
				{name: "pod-early-create", time: time.Unix(100, 0)},
			},
			events: []struct {
				podName string
				time    time.Time
			}{
				{podName: "pod-late-create", time: time.Unix(210, 0)},
				{podName: "pod-early-create", time: time.Unix(110, 0)},
			},
			wantScheduleTimes: map[string]time.Time{
				"default/pod-early-create": time.Unix(110, 0),
				"default/pod-late-create":  time.Unix(210, 0),
			},
		},
		{
			name:             "map by order - mismatch count",
			mapEventsByOrder: true,
			podCreates: []struct {
				name string
				time time.Time
			}{
				{name: "pod1", time: time.Unix(100, 0)},
			},
			events: []struct {
				podName string
				time    time.Time
			}{
				{podName: "event1", time: time.Unix(110, 0)},
				{podName: "event2", time: time.Unix(120, 0)},
			},
		},
		{
			name:             "filtering untracked pods",
			mapEventsByOrder: true,
			podCreates: []struct {
				name string
				time time.Time
			}{
				{name: "tracked-pod", time: time.Unix(100, 0)},
			},
			events: []struct {
				podName string
				time    time.Time
			}{
				{podName: "untracked-pod", time: time.Unix(110, 0)},
				{podName: "tracked-pod", time: time.Unix(120, 0)},
			},
			wantScheduleTimes: map[string]time.Time{
				"default/tracked-pod": time.Unix(120, 0),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := createPodStartupLatencyMeasurement().(*podStartupLatencyMeasurement)
			p.mapEventsByOrder = tc.mapEventsByOrder
			p.selector.Namespace = "default"

			for _, pc := range tc.podCreates {
				key := createMetaNamespaceKey("default", pc.name)
				p.podStartupEntries.Set(key, createPhase, pc.time)
			}

			client := fake.NewSimpleClientset()
			for _, e := range tc.events {
				event := &corev1.Event{
					ObjectMeta: metav1.ObjectMeta{
						Name:      e.podName + "-event",
						Namespace: "default",
					},
					InvolvedObject: corev1.ObjectReference{
						Kind:      "Pod",
						Name:      e.podName,
						Namespace: "default",
					},
					Source:    corev1.EventSource{Component: defaultSchedulerName},
					EventTime: metav1.MicroTime{Time: e.time},
				}
				_, err := client.CoreV1().Events("default").Create(context.TODO(), event, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create event: %v", err)
				}
			}

			err := p.gatherScheduleTimes(client, defaultSchedulerName)
			if (err != nil) != tc.wantErr {
				t.Errorf("gatherScheduleTimes() error = %v, wantErr %v", err, tc.wantErr)
				return
			}

			if !tc.wantErr {
				for key, wantTime := range tc.wantScheduleTimes {
					gotTime, exists := p.podStartupEntries.Get(key, schedulePhase)
					if !exists {
						t.Errorf("schedule time for %s not found", key)
						continue
					}
					if !gotTime.Equal(wantTime) {
						t.Errorf("schedule time for %s = %v, want %v", key, gotTime, wantTime)
					}
				}
			}
		})
	}
}

func TestProcessEvent(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	testCases := []struct {
		name              string
		pod               *corev1.Pod
		wantCreateTime   time.Time
		wantScheduleTime time.Time
		wantRunTime      time.Time
	}{
		{
			name: "scheduled and running pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pod1",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Second)),
				},
				Spec: corev1.PodSpec{
					NodeName: "node1",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{
									StartedAt: metav1.NewTime(now.Add(-5 * time.Second)),
								},
							},
						},
					},
				},
			},
			wantCreateTime:   now.Add(-10 * time.Second),
			wantScheduleTime: now, // Should be recvTime
			wantRunTime:      now.Add(-5 * time.Second),
		},
		{
			name: "scheduled but not yet running pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod2",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName: "node1",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			wantScheduleTime: now, // Should be recvTime
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := createPodStartupLatencyMeasurement().(*podStartupLatencyMeasurement)
			p.processEvent(&eventData{obj: tc.pod, recvTime: now})

			key := "default/" + tc.pod.Name
			if !tc.wantCreateTime.IsZero() {
				if got, _ := p.podStartupEntries.Get(key, createPhase); !got.Equal(tc.wantCreateTime) {
					t.Errorf("create time = %v, want %v", got, tc.wantCreateTime)
				}
			}
			if !tc.wantScheduleTime.IsZero() {
				if got, _ := p.podStartupEntries.Get(key, schedulePhase); !got.Equal(tc.wantScheduleTime) {
					t.Errorf("schedule time = %v, want %v", got, tc.wantScheduleTime)
				}
			}
			if !tc.wantRunTime.IsZero() {
				if got, _ := p.podStartupEntries.Get(key, runPhase); !got.Equal(tc.wantRunTime) {
					t.Errorf("run time = %v, want %v", got, tc.wantRunTime)
				}
			}
		})
	}
}
