// Code generated by mockery v1.0.0. DO NOT EDIT.

package mocks

import (
	context "context"

	kafka_go "github.com/segmentio/kafka-go"

	mock "github.com/stretchr/testify/mock"
)

// IWriter is an autogenerated mock type for the IWriter type
type IWriter struct {
	mock.Mock
}

// Close provides a mock function with given fields:
func (_m *IWriter) Close() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// WriteMessages provides a mock function with given fields: ctx, msgs
func (_m *IWriter) WriteMessages(ctx context.Context, msgs ...kafka_go.Message) error {
	_va := make([]interface{}, len(msgs))
	for _i := range msgs {
		_va[_i] = msgs[_i]
	}
	var _ca []interface{}
	_ca = append(_ca, ctx)
	_ca = append(_ca, _va...)
	ret := _m.Called(_ca...)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, ...kafka_go.Message) error); ok {
		r0 = rf(ctx, msgs...)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}
