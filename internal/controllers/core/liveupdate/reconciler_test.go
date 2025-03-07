package liveupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tilt-dev/tilt/internal/build"
	"github.com/tilt-dev/tilt/internal/containerupdate"
	"github.com/tilt-dev/tilt/internal/controllers/apis/configmap"
	"github.com/tilt-dev/tilt/internal/controllers/apis/liveupdate"
	"github.com/tilt-dev/tilt/internal/controllers/fake"
	"github.com/tilt-dev/tilt/internal/dockercompose"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/store/buildcontrols"
	"github.com/tilt-dev/tilt/pkg/apis"
	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"
	"github.com/tilt-dev/tilt/pkg/logger"
	"github.com/tilt-dev/tilt/pkg/model"
)

func TestIndexing(t *testing.T) {
	f := newFixture(t)

	// KubernetesDiscovery + KubernetesApply + ImageMap
	f.Create(&v1alpha1.LiveUpdate{
		ObjectMeta: metav1.ObjectMeta{Name: "all"},
		Spec: v1alpha1.LiveUpdateSpec{
			BasePath: "/tmp",
			Selector: v1alpha1.LiveUpdateSelector{
				Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
					DiscoveryName: "discovery",
					ApplyName:     "apply",
					ImageMapName:  "imagemap",
				},
			},
			Syncs: []v1alpha1.LiveUpdateSync{
				{LocalPath: "in", ContainerPath: "/out/"},
			},
		},
	})

	// KubernetesDiscovery ONLY [w/o Kubernetes Apply or ImageMap]
	f.Create(&v1alpha1.LiveUpdate{
		ObjectMeta: metav1.ObjectMeta{Name: "kdisco-only"},
		Spec: v1alpha1.LiveUpdateSpec{
			BasePath: "/tmp",
			Selector: v1alpha1.LiveUpdateSelector{
				Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
					DiscoveryName: "discovery",
					ContainerName: "foo",
				},
			},
			Syncs: []v1alpha1.LiveUpdateSync{
				{LocalPath: "in", ContainerPath: "/out/"},
			},
		},
	})

	reqs := f.r.indexer.Enqueue(&v1alpha1.KubernetesDiscovery{ObjectMeta: metav1.ObjectMeta{Name: "discovery"}})
	require.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "all"}},
		{NamespacedName: types.NamespacedName{Name: "kdisco-only"}},
	}, reqs, "KubernetesDiscovery indexing")

	reqs = f.r.indexer.Enqueue(&v1alpha1.KubernetesApply{ObjectMeta: metav1.ObjectMeta{Name: "apply"}})
	require.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "all"}},
	}, reqs, "KubernetesApply indexing")

	reqs = f.r.indexer.Enqueue(&v1alpha1.ImageMap{ObjectMeta: metav1.ObjectMeta{Name: "imagemap"}})
	require.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "all"}},
	}, reqs, "ImageMap indexing")
}

func TestMissingApply(t *testing.T) {
	f := newFixture(t)

	f.setupFrontend()
	f.Delete(&v1alpha1.KubernetesApply{ObjectMeta: metav1.ObjectMeta{Name: "frontend-apply"}})
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "ObjectNotFound", lu.Status.Failed.Reason)
		assert.NotContains(t, f.Stdout(), "ObjectNotFound")
	}

	f.assertSteadyState(&lu)
}

func TestConsumeFileEvents(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()

	// Verify initial setup.
	m, ok := f.r.monitors["frontend-liveupdate"]
	require.True(t, ok)
	assert.Equal(t, map[string]*monitorSource{}, m.sources)
	assert.Equal(t, "frontend-discovery", m.lastKubernetesDiscovery.Name)
	assert.Nil(t, f.st.lastStartedAction)

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	// Also make sure the sync gets pulled into the monitor.
	assert.Equal(t, map[string]metav1.MicroTime{
		txtPath: txtChangeTime,
	}, m.sources["frontend-fw"].modTimeByPath)
	assert.Equal(t, 1, len(f.cu.Calls))

	// re-reconcile, and make sure we don't try to resync.
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	assert.Equal(t, 1, len(f.cu.Calls))

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)

	if assert.NotNil(t, f.st.lastStartedAction) {
		assert.Equal(t, []string{txtPath}, f.st.lastStartedAction.FilesChanged)
	}
	assert.NotNil(t, f.st.lastCompletedAction)
}

func TestConsumeFileEventsDockerCompose(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupDockerComposeFrontend()

	// Verify initial setup.
	m, ok := f.r.monitors["frontend-liveupdate"]
	require.True(t, ok)
	assert.Equal(t, map[string]*monitorSource{}, m.sources)
	assert.Equal(t, "frontend-service", m.lastDockerComposeService.Name)
	assert.Nil(t, f.st.lastStartedAction)

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	// Also make sure the sync gets pulled into the monitor.
	assert.Equal(t, map[string]metav1.MicroTime{
		txtPath: txtChangeTime,
	}, m.sources["frontend-fw"].modTimeByPath)
	assert.Equal(t, 1, len(f.cu.Calls))

	// re-reconcile, and make sure we don't try to resync.
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	assert.Equal(t, 1, len(f.cu.Calls))

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)

	if assert.NotNil(t, f.st.lastStartedAction) {
		assert.Equal(t, []string{txtPath}, f.st.lastStartedAction.FilesChanged)
	}
	assert.NotNil(t, f.st.lastCompletedAction)

	// Make sure the container was NOT restarted.
	if assert.Equal(t, 1, len(f.cu.Calls)) {
		assert.True(t, f.cu.Calls[0].HotReload)
	}

	f.assertSteadyState(&lu)

	// Docker Compose containers can be restarted in-place,
	// preserving their filesystem.
	dc := &v1alpha1.DockerComposeService{}
	f.MustGet(types.NamespacedName{Name: "frontend-service"}, dc)
	dc = dc.DeepCopy()
	dc.Status.ContainerState.StartedAt = apis.NowMicro()
	f.UpdateStatus(dc)

	f.assertSteadyState(&lu)
}

func TestConsumeFileEventsUpdateModeManual(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	lu.Annotations[liveupdate.AnnotationUpdateMode] = liveupdate.UpdateModeManual
	f.Update(&lu)

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, "Trigger", lu.Status.Containers[0].Waiting.Reason)
	}

	f.Upsert(&v1alpha1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configmap.TriggerQueueName,
		},
		Data: map[string]string{
			"0-name": "frontend",
		},
	})

	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}
}

func TestWaitingContainer(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()
	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Waiting: &v1alpha1.ContainerStateWaiting{},
						},
					},
				},
			},
		},
	})

	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, "ContainerWaiting", lu.Status.Containers[0].Waiting.Reason)
	}
	assert.Equal(t, 0, len(f.cu.Calls))

	f.assertSteadyState(&lu)

	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	})

	// Re-reconcile, and make sure we pull in the files.
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	assert.Equal(t, 1, len(f.cu.Calls))
}

func TestWaitingContainerNoID(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()
	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				InitContainers: []v1alpha1.Container{
					{
						Name:  "main-init",
						ID:    "main-id",
						Image: "busybox",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Waiting: &v1alpha1.ContainerStateWaiting{Reason: "PodInitializing"},
						},
					},
				},
			},
		},
	})

	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, "ContainerWaiting", lu.Status.Containers[0].Waiting.Reason)
	}
	assert.Equal(t, 0, len(f.cu.Calls))

	f.assertSteadyState(&lu)
}

func TestOneTerminatedContainer(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()
	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Terminated: &v1alpha1.ContainerStateTerminated{},
						},
					},
				},
			},
		},
	})

	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "Terminated", lu.Status.Failed.Reason)
		assert.Contains(t, f.Stdout(),
			`LiveUpdate "frontend-liveupdate" Terminated: Container for live update is stopped. Pod name: pod-1`)
	}

	f.assertSteadyState(&lu)
}

func TestOneRunningOneTerminatedContainer(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()
	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Terminated: &v1alpha1.ContainerStateTerminated{},
						},
					},
				},
			},
			{
				Name:      "pod-2",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	})

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	// Also make sure the sync gets pulled into the monitor.
	m, ok := f.r.monitors["frontend-liveupdate"]
	require.True(t, ok)
	assert.Equal(t, map[string]metav1.MicroTime{
		txtPath: txtChangeTime,
	}, m.sources["frontend-fw"].modTimeByPath)
	assert.Equal(t, 1, len(f.cu.Calls))
	assert.Equal(t, "pod-2", f.cu.Calls[0].ContainerInfo.PodID.String())

	f.assertSteadyState(&lu)
}

func TestCrashLoopBackoff(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()
	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Waiting: &v1alpha1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
						},
					},
				},
			},
		},
	})

	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "CrashLoopBackOff", lu.Status.Failed.Reason)
	}
	assert.Equal(t, 0, len(f.cu.Calls))

	f.assertSteadyState(&lu)

	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	})

	// CrashLoopBackOff is a permanent state. If the container starts running
	// again, we don't "revive" the live-update.
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "CrashLoopBackOff", lu.Status.Failed.Reason)
	}
}

func TestStopPathConsumedByImageBuild(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	stopPath := filepath.Join(p, "stop.txt")
	stopChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupFrontend()

	f.addFileEvent("frontend-fw", stopPath, stopChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "UpdateStopped", lu.Status.Failed.Reason)
	}

	f.assertSteadyState(&lu)

	// Clear the failure with an Image build
	f.Upsert(&v1alpha1.ImageMap{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-image-map"},
		Status: v1alpha1.ImageMapStatus{
			Image:            "frontend-image:my-tag",
			ImageFromCluster: "local-registry:12345/frontend-image:my-tag",
			BuildStartTime:   &metav1.MicroTime{Time: nowMicro.Add(2 * time.Second)},
		},
	})

	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)

	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(3 * time.Second)}
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)

	assert.Equal(t, 0, len(f.cu.Calls))
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	assert.Equal(t, 1, len(f.cu.Calls))
}

func TestStopPathConsumedByKubernetesApply(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	stopPath := filepath.Join(p, "stop.txt")
	stopChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	// we are going to delete the ImageMap, so we cannot use it as a selector
	// (the default behavior)
	f.setupFrontendWithSelector(&v1alpha1.LiveUpdateSelector{
		Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
			DiscoveryName: "frontend-discovery",
			ApplyName:     "frontend-apply",
			Image:         "local-registry:12345/frontend-image:some-tag",
		},
	})
	f.Delete(&v1alpha1.ImageMap{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-image-map"},
	})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	lu.Spec.Sources[0].ImageMap = ""
	f.Update(&lu)

	f.addFileEvent("frontend-fw", stopPath, stopChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "UpdateStopped", lu.Status.Failed.Reason)
	}

	f.assertSteadyState(&lu)

	// Clear the failure with an Apply
	f.Upsert(&v1alpha1.KubernetesApply{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-apply"},
		Status: v1alpha1.KubernetesApplyStatus{
			LastApplyStartTime: metav1.MicroTime{Time: nowMicro.Add(2 * time.Second)},
		},
	})

	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)

	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(3 * time.Second)}
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)

	assert.Equal(t, 0, len(f.cu.Calls))
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})
	assert.Equal(t, 1, len(f.cu.Calls))
}

func TestKubernetesContainerNameSelector(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	// change from default ImageMap selector to a container name selector
	f.setupFrontendWithSelector(&v1alpha1.LiveUpdateSelector{
		Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
			DiscoveryName: "frontend-discovery",
			ApplyName:     "frontend-apply",
			ContainerName: "main",
		},
	})

	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "frontend-image",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	})

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Equal(t, "main", lu.Spec.Selector.Kubernetes.ContainerName)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	f.assertSteadyState(&lu)
}

func TestKubernetesImageSelector(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	// change from default ImageMap selector to an image selector
	f.setupFrontendWithSelector(&v1alpha1.LiveUpdateSelector{
		Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
			DiscoveryName: "frontend-discovery",
			ApplyName:     "frontend-apply",
			Image:         "local-registry:12345/frontend-image:some-tag",
		},
	})

	f.kdUpdateStatus("frontend-discovery", v1alpha1.KubernetesDiscoveryStatus{
		Pods: []v1alpha1.Pod{
			{
				Name:      "pod-1",
				Namespace: "default",
				Containers: []v1alpha1.Container{
					{
						Name:  "main",
						ID:    "main-id",
						Image: "local-registry:12345/frontend-image:my-tag",
						State: v1alpha1.ContainerState{
							Running: &v1alpha1.ContainerStateRunning{},
						},
					},
				},
			},
		},
	})

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Equal(t, "local-registry:12345/frontend-image:some-tag",
		lu.Spec.Selector.Kubernetes.Image)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	f.assertSteadyState(&lu)
}

func TestDockerComposeRestartPolicy(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupDockerComposeFrontend()

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	lu.Spec.Restart = v1alpha1.LiveUpdateRestartStrategyAlways
	f.Upsert(&lu)

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	// Make sure the container was restarted.
	if assert.Equal(t, 1, len(f.cu.Calls)) {
		assert.False(t, f.cu.Calls[0].HotReload)
	}
}

func TestDockerComposeExecs(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupDockerComposeFrontend()

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)

	execs := []v1alpha1.LiveUpdateExec{
		{Args: model.ToUnixCmd("./foo.sh bar").Argv},
		{Args: model.ToUnixCmd("yarn install").Argv, TriggerPaths: []string{"a.txt"}},
		{Args: model.ToUnixCmd("pip install").Argv, TriggerPaths: []string{"requirements.txt"}},
	}
	lu.Spec.Execs = execs
	f.Upsert(&lu)

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, txtChangeTime, lu.Status.Containers[0].LastFileTimeSynced)
	}

	// Make sure there were no exec errors.
	if assert.NotNil(t, f.st.lastCompletedAction) {
		assert.Nil(t, f.st.lastCompletedAction.Error)
	}

	// Make sure two cmds were executed, and one was skipped.
	if assert.Equal(t, 1, len(f.cu.Calls)) {
		assert.Equal(t, []model.Cmd{
			model.ToUnixCmd("./foo.sh bar"),
			model.ToUnixCmd("yarn install"),
		}, f.cu.Calls[0].Cmds)
	}
}

func TestDockerComposeExecInfraFailure(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupDockerComposeFrontend()

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)

	execs := []v1alpha1.LiveUpdateExec{
		{Args: model.ToUnixCmd("echo error && exit 1").Argv},
	}
	lu.Spec.Execs = execs
	f.Upsert(&lu)

	f.cu.SetUpdateErr(fmt.Errorf("cluster connection lost"))

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	if assert.NotNil(t, lu.Status.Failed) {
		assert.Equal(t, "UpdateFailed", lu.Status.Failed.Reason)
		assert.Equal(t, "Updating container main-id: cluster connection lost",
			lu.Status.Failed.Message)
	}

	// Make sure there were  exec errors.
	if assert.NotNil(t, f.st.lastCompletedAction) {
		assert.Equal(t, "Updating container main-id: cluster connection lost",
			f.st.lastCompletedAction.Error.Error())
	}
}

func TestDockerComposeExecRunFailure(t *testing.T) {
	f := newFixture(t)

	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()
	txtPath := filepath.Join(p, "a.txt")
	txtChangeTime := metav1.MicroTime{Time: nowMicro.Add(time.Second)}

	f.setupDockerComposeFrontend()

	var lu v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)

	execs := []v1alpha1.LiveUpdateExec{
		{Args: model.ToUnixCmd("echo error && exit 1").Argv},
	}
	lu.Spec.Execs = execs
	f.Upsert(&lu)

	f.cu.SetUpdateErr(build.NewRunStepFailure(errors.New("compilation failed")))

	// Trigger a file event, and make sure that the status reflects the sync.
	f.addFileEvent("frontend-fw", txtPath, txtChangeTime)
	f.MustReconcile(types.NamespacedName{Name: "frontend-liveupdate"})

	f.MustGet(types.NamespacedName{Name: "frontend-liveupdate"}, &lu)
	assert.Nil(t, lu.Status.Failed)
	if assert.Equal(t, 1, len(lu.Status.Containers)) {
		assert.Equal(t, "compilation failed", lu.Status.Containers[0].LastExecError)
	}

	// Make sure there were  exec errors.
	if assert.NotNil(t, f.st.lastCompletedAction) {
		assert.Equal(t, "compilation failed",
			f.st.lastCompletedAction.Error.Error())
	}
}

type TestingStore struct {
	*store.TestingStore
	ctx                 context.Context
	lastStartedAction   *buildcontrols.BuildStartedAction
	lastCompletedAction *buildcontrols.BuildCompleteAction
}

func newTestingStore() *TestingStore {
	return &TestingStore{TestingStore: store.NewTestingStore()}
}

func (s *TestingStore) Dispatch(action store.Action) {
	s.TestingStore.Dispatch(action)
	switch action := action.(type) {
	case buildcontrols.BuildStartedAction:
		s.lastStartedAction = &action
	case buildcontrols.BuildCompleteAction:
		s.lastCompletedAction = &action
	case store.LogAction:
		_, _ = logger.Get(s.ctx).Writer(action.Level()).Write(action.Message())
	}
}

type fixture struct {
	*fake.ControllerFixture
	r  *Reconciler
	cu *containerupdate.FakeContainerUpdater
	st *TestingStore
}

func newFixture(t testing.TB) *fixture {
	cfb := fake.NewControllerFixtureBuilder(t)
	cu := &containerupdate.FakeContainerUpdater{}
	st := newTestingStore()
	r := NewFakeReconciler(st, cu, cfb.Client)
	cf := cfb.Build(r)
	st.ctx = cf.Context()
	return &fixture{
		ControllerFixture: cf,
		r:                 r,
		cu:                cu,
		st:                st,
	}
}

func (f *fixture) addFileEvent(name string, p string, time metav1.MicroTime) {
	var fw v1alpha1.FileWatch
	f.MustGet(types.NamespacedName{Name: name}, &fw)
	update := fw.DeepCopy()
	update.Status.FileEvents = append(update.Status.FileEvents, v1alpha1.FileEvent{
		Time:      time,
		SeenFiles: []string{p},
	})
	f.UpdateStatus(update)
}

func (f *fixture) setupFrontend() {
	f.setupFrontendWithSelector(nil)
}

// Create a frontend LiveUpdate with all objects attached.
func (f *fixture) setupFrontendWithSelector(selector *v1alpha1.LiveUpdateSelector) {
	p, _ := os.Getwd()
	now := apis.Now()
	nowMicro := apis.NowMicro()

	// Create all the objects.
	f.Create(&v1alpha1.FileWatch{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-fw"},
		Spec: v1alpha1.FileWatchSpec{
			WatchedPaths: []string{p},
		},
		Status: v1alpha1.FileWatchStatus{
			MonitorStartTime: nowMicro,
		},
	})
	f.Create(&v1alpha1.KubernetesApply{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-apply"},
		Status:     v1alpha1.KubernetesApplyStatus{},
	})
	f.Create(&v1alpha1.ImageMap{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-image-map"},
		Status: v1alpha1.ImageMapStatus{
			Image:            "frontend-image:my-tag",
			ImageFromCluster: "local-registry:12345/frontend-image:my-tag",
			BuildStartTime:   &nowMicro,
		},
	})
	f.Create(&v1alpha1.KubernetesDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-discovery"},
		Status: v1alpha1.KubernetesDiscoveryStatus{
			MonitorStartTime: nowMicro,
			Pods: []v1alpha1.Pod{
				{
					Name:      "pod-1",
					Namespace: "default",
					Containers: []v1alpha1.Container{
						{
							Name:  "main",
							ID:    "main-id",
							Image: "local-registry:12345/frontend-image:my-tag",
							Ready: true,
							State: v1alpha1.ContainerState{
								Running: &v1alpha1.ContainerStateRunning{
									StartedAt: now,
								},
							},
						},
					},
				},
			},
		},
	})

	if selector == nil {
		// default selector matches the most common Tilt use-case, which has
		// KDisco + KApply and selects via ImageMap
		selector = &v1alpha1.LiveUpdateSelector{
			Kubernetes: &v1alpha1.LiveUpdateKubernetesSelector{
				ApplyName:     "frontend-apply",
				DiscoveryName: "frontend-discovery",
				ImageMapName:  "frontend-image-map",
			},
		}
	}

	f.Create(&v1alpha1.LiveUpdate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "frontend-liveupdate",
			Annotations: map[string]string{
				v1alpha1.AnnotationManifest:     "frontend",
				liveupdate.AnnotationUpdateMode: "auto",
			},
		},
		Spec: v1alpha1.LiveUpdateSpec{
			BasePath: p,
			Sources: []v1alpha1.LiveUpdateSource{{
				FileWatch: "frontend-fw",
				ImageMap:  "frontend-image-map",
			}},
			Selector: *selector,
			Syncs: []v1alpha1.LiveUpdateSync{
				{LocalPath: ".", ContainerPath: "/app"},
			},
			StopPaths: []string{"stop.txt"},
		},
	})
	f.Create(&v1alpha1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configmap.TriggerQueueName,
		},
	})
}

// Create a frontend DockerCompose LiveUpdate with all objects attached.
func (f *fixture) setupDockerComposeFrontend() {
	p, _ := os.Getwd()
	nowMicro := apis.NowMicro()

	// Create all the objects.
	f.Create(&v1alpha1.FileWatch{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-fw"},
		Spec: v1alpha1.FileWatchSpec{
			WatchedPaths: []string{p},
		},
		Status: v1alpha1.FileWatchStatus{
			MonitorStartTime: nowMicro,
		},
	})
	f.Create(&v1alpha1.DockerComposeService{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-service"},
		Status: v1alpha1.DockerComposeServiceStatus{
			ContainerID: "main-id",
			ContainerState: &v1alpha1.DockerContainerState{
				Status:    dockercompose.ContainerStatusRunning,
				StartedAt: nowMicro,
			},
		},
	})
	f.Create(&v1alpha1.ImageMap{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-image-map"},
		Status: v1alpha1.ImageMapStatus{
			Image:            "frontend-image:my-tag",
			ImageFromCluster: "frontend-image:my-tag",
			BuildStartTime:   &nowMicro,
		},
	})
	f.Create(&v1alpha1.LiveUpdate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "frontend-liveupdate",
			Annotations: map[string]string{
				v1alpha1.AnnotationManifest:     "frontend",
				liveupdate.AnnotationUpdateMode: "auto",
			},
		},
		Spec: v1alpha1.LiveUpdateSpec{
			BasePath: p,
			Sources: []v1alpha1.LiveUpdateSource{{
				FileWatch: "frontend-fw",
				ImageMap:  "frontend-image-map",
			}},
			Selector: v1alpha1.LiveUpdateSelector{
				DockerCompose: &v1alpha1.LiveUpdateDockerComposeSelector{
					Service: "frontend-service",
				},
			},
			Syncs: []v1alpha1.LiveUpdateSync{
				{LocalPath: ".", ContainerPath: "/app"},
			},
			StopPaths: []string{"stop.txt"},
		},
	})
	f.Create(&v1alpha1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configmap.TriggerQueueName,
		},
	})
}

func (f *fixture) assertSteadyState(lu *v1alpha1.LiveUpdate) {
	startCalls := len(f.cu.Calls)

	f.T().Helper()
	f.MustReconcile(types.NamespacedName{Name: lu.Name})
	var lu2 v1alpha1.LiveUpdate
	f.MustGet(types.NamespacedName{Name: lu.Name}, &lu2)
	assert.Equal(f.T(), lu.ResourceVersion, lu2.ResourceVersion)

	assert.Equal(f.T(), startCalls, len(f.cu.Calls))
}

func (f *fixture) kdUpdateStatus(name string, status v1alpha1.KubernetesDiscoveryStatus) {
	var kd v1alpha1.KubernetesDiscovery
	f.MustGet(types.NamespacedName{Name: name}, &kd)
	update := kd.DeepCopy()
	update.Status = status
	f.UpdateStatus(update)
}
