package cmd

type nativeUDETransport interface {
	Done() <-chan error
	Close() error
}
