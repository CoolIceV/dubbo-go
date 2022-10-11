// Code generated by MockGen. DO NOT EDIT.
// Source: ./rolling_metrics.go

// Package rolling is a generated GoMock package.
package rolling

import (
	reflect "reflect"
)

import (
	gomock "github.com/golang/mock/gomock"
)

import (
	common "dubbo.apache.org/dubbo-go/v3/common"
)

// MockMetrics is a mock of Metrics interface.
type MockMetrics struct {
	ctrl     *gomock.Controller
	recorder *MockMetricsMockRecorder
}

// MockMetricsMockRecorder is the mock recorder for MockMetrics.
type MockMetricsMockRecorder struct {
	mock *MockMetrics
}

// NewMockMetrics creates a new mock instance.
func NewMockMetrics(ctrl *gomock.Controller) *MockMetrics {
	mock := &MockMetrics{ctrl: ctrl}
	mock.recorder = &MockMetricsMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMetrics) EXPECT() *MockMetricsMockRecorder {
	return m.recorder
}

// AppendMethodMetrics mocks base method.
func (m *MockMetrics) AppendMethodMetrics(url *common.URL, methodName, key string, value float64) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "AppendMethodMetrics", url, methodName, key, value)
	ret0, _ := ret[0].(error)
	return ret0
}

// AppendMethodMetrics indicates an expected call of AppendMethodMetrics.
func (mr *MockMetricsMockRecorder) AppendMethodMetrics(url, methodName, key, value interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "AppendMethodMetrics", reflect.TypeOf((*MockMetrics)(nil).AppendMethodMetrics), url, methodName, key, value)
}

// GetMethodMetrics mocks base method.
func (m *MockMetrics) GetMethodMetrics(url *common.URL, methodName, key string) (float64, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetMethodMetrics", url, methodName, key)
	ret0, _ := ret[0].(float64)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetMethodMetrics indicates an expected call of GetMethodMetrics.
func (mr *MockMetricsMockRecorder) GetMethodMetrics(url, methodName, key interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetMethodMetrics", reflect.TypeOf((*MockMetrics)(nil).GetMethodMetrics), url, methodName, key)
}
