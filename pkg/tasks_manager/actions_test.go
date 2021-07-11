package tasks_manager

import (
	"context"
	"testing"

	"github.com/hashicorp/vault/sdk/logical"
	"github.com/stretchr/testify/assert"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/tasks_manager/worker"
)

// check that Manager.RunTask queues task or returns the busy error
func TestManager_RunTask(t *testing.T) {
	ctx, m, storage := setupTest()

	assert.Nil(t, m.Storage, "should be initialized on the first action call")
	var uuids []string
	// check the first task
	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		assert.NotNil(t, m.Storage, "should be initialized on the first action call")

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check the second task
	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		if assert.Error(t, err) {
			assert.Equal(t, err, BusyError)
		}
		assert.Empty(t, uuid)
	}

	// check queue
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.RunTask queues task or returns the busy error
func TestManager_RunTaskWithCurrentRunningTask(t *testing.T) {
	ctx, m, storage := setupTest()

	m.Storage = storage
	err := storage.Put(ctx, &logical.StorageEntry{
		Key:   storageKeyCurrentRunningTask,
		Value: []byte("ANY"),
	})
	assert.Nil(t, err)

	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		if assert.Error(t, err) {
			assert.Equal(t, err, BusyError)
		}
		assert.Empty(t, uuid)
		assert.NotNil(t, m.Storage, "should be initialized on the first action call")
	}

	err = storage.Delete(ctx, storageKeyCurrentRunningTask)
	assert.Nil(t, err)

	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.RunTask invalidates inconsistent storage
func TestManager_RunTaskInvalidateStorage(t *testing.T) {
	ctx, m, storage := setupTest()

	// imitate inconsistent storage condition after previous plugin run
	var runningTaskUUID string
	var queuedTaskUUID string
	{
		runningTask := newTask()
		runningTaskUUID = runningTask.UUID
		runningTask.Status = taskStatusRunning
		err := putTaskIntoStorage(ctx, storage, runningTask)
		assert.Nil(t, err)

		err = storage.Put(ctx, &logical.StorageEntry{
			Key:   storageKeyCurrentRunningTask,
			Value: []byte(runningTask.UUID),
		})
		assert.Nil(t, err)

		queuedTask := newTask()
		queuedTaskUUID = queuedTask.UUID
		queuedTask.Status = taskStatusQueued
		err = putQueuedTaskIntoStorage(ctx, storage, queuedTask)
		assert.Nil(t, err)
	}

	assert.Nil(t, m.Storage, "should be initialized on the first action call")

	uuid, err := m.RunTask(ctx, storage, noneTask)
	assert.Nil(t, err)
	assert.NotEmpty(t, uuid)

	// check running task invalidation
	{
		task, err := getTaskFromStorage(ctx, storage, runningTaskUUID)
		assert.Nil(t, err)
		if assert.NotNil(t, task) {
			assert.Equal(t, taskStatusFailed, task.Status)
			assert.Equal(t, taskReasonInvalidatedTask, task.Reason)
		}

		currentTaskUUID, err := getCurrentTaskUUIDFromStorage(ctx, storage)
		assert.Nil(t, err)
		assert.Empty(t, currentTaskUUID)
	}

	// check queue task invalidation
	{
		task, err := getQueuedTaskFromStorage(ctx, storage, queuedTaskUUID)
		assert.Nil(t, err)
		assert.Nil(t, task)

		task, err = getTaskFromStorage(ctx, storage, queuedTaskUUID)
		assert.Nil(t, err)
		if assert.NotNil(t, task) {
			assert.Equal(t, taskStatusFailed, task.Status)
			assert.Equal(t, taskReasonInvalidatedTask, task.Reason)
		}
	}
}

// check that Manager.AddTask queues all tasks
func TestManager_AddTask(t *testing.T) {
	ctx, m, storage := setupTest()

	assert.Nil(t, m.Storage, "should be initialized on the first action call")
	var uuids []string
	for i := 0; i < 2; i++ {
		uuid, err := m.AddTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		if i == 0 {
			assert.NotNil(t, m.Storage, "should be initialized on the first action call")
		}

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check queue and task order
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.AddOptionalTask queues task when queue is empty
func TestManager_AddOptionalTask(t *testing.T) {
	ctx, m, storage := setupTest()

	assert.Nil(t, m.Storage, "should be initialized on the first action call")
	var uuids []string
	// check the first task
	{
		uuid, added, err := m.AddOptionalTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		assert.True(t, added)
		assert.NotNil(t, m.Storage, "should be initialized on the first action call")

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check the second task
	{
		uuid, added, err := m.AddOptionalTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.Empty(t, uuid)
		assert.False(t, added)
	}

	// check queue
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

func setupTest() (context.Context, *Manager, logical.Storage) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	return ctx, m, storage
}

func initManagerWithoutWorker() *Manager {
	taskChan := make(chan *worker.Task, taskChanSize)
	m := &Manager{taskChan: taskChan}
	return m
}

func noneTask(_ context.Context, _ logical.Storage) error { return nil }

func assertQueuedTaskInStorage(t *testing.T, ctx context.Context, storage logical.Storage, uuid string) {
	task, err := getQueuedTaskFromStorage(ctx, storage, uuid)
	assert.Nil(t, err)
	assert.NotNil(t, task)
	assert.Equal(t, task.Status, taskStatusQueued)
}
