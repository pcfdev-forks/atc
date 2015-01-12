// This file was generated by counterfeiter
package fakes

import (
	"sync"

	"github.com/concourse/atc/worker"
)

type FakeWorkerProvider struct {
	WorkersStub        func() ([]worker.Worker, error)
	workersMutex       sync.RWMutex
	workersArgsForCall []struct{}
	workersReturns struct {
		result1 []worker.Worker
		result2 error
	}
}

func (fake *FakeWorkerProvider) Workers() ([]worker.Worker, error) {
	fake.workersMutex.Lock()
	fake.workersArgsForCall = append(fake.workersArgsForCall, struct{}{})
	fake.workersMutex.Unlock()
	if fake.WorkersStub != nil {
		return fake.WorkersStub()
	} else {
		return fake.workersReturns.result1, fake.workersReturns.result2
	}
}

func (fake *FakeWorkerProvider) WorkersCallCount() int {
	fake.workersMutex.RLock()
	defer fake.workersMutex.RUnlock()
	return len(fake.workersArgsForCall)
}

func (fake *FakeWorkerProvider) WorkersReturns(result1 []worker.Worker, result2 error) {
	fake.WorkersStub = nil
	fake.workersReturns = struct {
		result1 []worker.Worker
		result2 error
	}{result1, result2}
}

var _ worker.WorkerProvider = new(FakeWorkerProvider)