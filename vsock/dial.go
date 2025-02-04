// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package vsock

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type config struct {
	logger            logrus.FieldLogger
	DialTimeout       time.Duration
	RetryTimeout      time.Duration
	RetryInterval     time.Duration
	ConnectMsgTimeout time.Duration
	AckMsgTimeout     time.Duration
}

func defaultConfig() config {
	noop := logrus.New()
	noop.Out = ioutil.Discard

	return config{
		DialTimeout:       100 * time.Millisecond,
		RetryTimeout:      20 * time.Second,
		RetryInterval:     100 * time.Millisecond,
		ConnectMsgTimeout: 100 * time.Millisecond,
		AckMsgTimeout:     1 * time.Second,
		logger:            noop,
	}
}

type DialOption func(c *config)

func WithDialTimeout(d time.Duration) DialOption {
	return func(c *config) {
		c.DialTimeout = d
	}
}

func WithRetryTimeout(d time.Duration) DialOption {
	return func(c *config) {
		c.RetryTimeout = d
	}
}

func WithRetryInterval(d time.Duration) DialOption {
	return func(c *config) {
		c.RetryInterval = d
	}
}

func WithConnectionMsgTimeout(d time.Duration) DialOption {
	return func(c *config) {
		c.ConnectMsgTimeout = d
	}
}

func WithAckMsgTimeout(d time.Duration) DialOption {
	return func(c *config) {
		c.AckMsgTimeout = d
	}
}

func WithLogger(logger logrus.FieldLogger) DialOption {
	return func(c *config) {
		c.logger = logger
	}
}

// Dial connects to the Firecracker host-side vsock at the provided unix path and port.
//
// It will retry connect attempts if a temporary error is encountered up to a fixed
// timeout or the provided request is canceled.
//
// path specifies the file system path of the UNIX domain socket.
//
// port will be used in the connect message to the firecracker vsock.
func DialContext(ctx context.Context, path string, port uint32, opts ...DialOption) (net.Conn, error) {
	t := defaultConfig()
	for _, o := range opts {
		o(&t)
	}

	return dial(ctx, path, port, t)
}

// Dial connects to the Firecracker host-side vsock at the provided unix path and port.
//
// See func Dial for a description of the path and port parameters.
func Dial(path string, port uint32, opts ...DialOption) (net.Conn, error) {
	return DialContext(context.Background(), path, port, opts...)
}

func dial(ctx context.Context, udsPath string, port uint32, c config) (net.Conn, error) {
	ticker := time.NewTicker(c.RetryInterval)
	defer ticker.Stop()

	logger := c.logger

	tickerCh := ticker.C
	var attemptCount int
	for {
		attemptCount++
		logger := logger.WithField("attempt", attemptCount)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tickerCh:
			conn, err := tryConnect(logger, udsPath, port, c)
			if isTemporaryNetErr(err) {
				err = errors.Wrap(err, "temporary vsock dial failure")
				logger.WithError(err).Debug()
				continue
			} else if err != nil {
				err = errors.Wrap(err, "non-temporary vsock dial failure")
				logger.WithError(err).Error()
				return nil, err
			}

			return conn, nil
		}
	}
}

func connectMsg(port uint32) string {
	// The message a host-side connection must write after connecting to a firecracker
	// vsock unix socket in order to establish a connection with a guest-side listener
	// at the provided port number. This is specified in Firecracker documentation:
	// https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md#host-initiated-connections
	return fmt.Sprintf("CONNECT %d\n", port)
}

// tryConnect attempts to dial a guest vsock listener at the provided host-side
// unix socket and provided guest-listener port.
func tryConnect(logger *logrus.Entry, udsPath string, port uint32, c config) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", udsPath, c.DialTimeout)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial %q within %s", udsPath, c.DialTimeout)
	}

	defer func() {
		if err != nil {
			closeErr := conn.Close()
			if closeErr != nil {
				logger.WithError(closeErr).Error(
					"failed to close vsock socket after previous error")
			}
		}
	}()

	msg := connectMsg(port)
	err = tryConnWrite(conn, msg, c.ConnectMsgTimeout)
	if err != nil {
		return nil, connectMsgError{
			cause: errors.Wrapf(err, `failed to write %q within %s`, msg, c.ConnectMsgTimeout),
		}
	}

	line, err := tryConnReadUntil(conn, '\n', c.AckMsgTimeout)
	if err != nil {
		return nil, ackError{
			cause: errors.Wrapf(err, `failed to read "OK <port>" within %s`, c.AckMsgTimeout),
		}
	}

	// The line would be "OK <assigned_hostside_port>\n", but we don't use the hostside port here.
	// https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md#host-initiated-connections
	if !strings.HasPrefix(line, "OK ") {
		return nil, ackError{
			cause: errors.Errorf(`expected to read "OK <port>", but instead read %q`, line),
		}
	}
	return conn, nil
}

// tryConnReadUntil will try to do a read from the provided conn until the specified
// end character is encounteed. Returning an error if the read does not complete
// within the provided timeout. It will reset socket deadlines to none after returning.
// It's only intended to be used for connect/ack messages, not general purpose reads
// after the vsock connection is established fully.
func tryConnReadUntil(conn net.Conn, end byte, timeout time.Duration) (string, error) {
	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	return bufio.NewReaderSize(conn, 32).ReadString(end)
}

// tryConnWrite will try to do a write to the provided conn, returning an error if
// the write fails, is partial or does not complete within the provided timeout. It
// will reset socket deadlines to none after returning. It's only intended to be
// used for connect/ack messages, not general purpose writes after the vsock
// connection is established fully.
func tryConnWrite(conn net.Conn, expectedWrite string, timeout time.Duration) error {
	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	bytesWritten, err := conn.Write([]byte(expectedWrite))
	if err != nil {
		return err
	}
	if bytesWritten != len(expectedWrite) {
		return errors.Errorf("incomplete write, expected %d bytes but wrote %d",
			len(expectedWrite), bytesWritten)
	}

	return nil
}

type connectMsgError struct {
	cause error
}

func (e connectMsgError) Error() string {
	return errors.Wrap(e.cause, "vsock connect message failure").Error()
}

func (e connectMsgError) Temporary() bool {
	return false
}

func (e connectMsgError) Timeout() bool {
	return false
}

type ackError struct {
	cause error
}

func (e ackError) Error() string {
	return errors.Wrap(e.cause, "vsock ack message failure").Error()
}

func (e ackError) Temporary() bool {
	return true
}

func (e ackError) Timeout() bool {
	return false
}

// isTemporaryNetErr returns whether the provided error is a retriable error.
func isTemporaryNetErr(err error) bool {
	var netError net.Error
	return err != nil && errors.As(err, &netError) && netError.Temporary()
}
