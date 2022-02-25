package connect

// errorTranslatingSender wraps a Sender to ensure that we always return coded
// errors to clients and write coded errors to the network.
type errorTranslatingSender struct {
	Sender

	toWire   func(error) error
	fromWire func(error) error
}

func (s *errorTranslatingSender) Send(msg any) error {
	return s.fromWire(s.Sender.Send(msg))
}

func (s *errorTranslatingSender) Close(err error) error {
	sendErr := s.Sender.Close(s.toWire(err))
	return s.fromWire(sendErr)
}

// errorTranslatingReceiver wraps a Receiver to make sure that we always return
// coded errors from clients.
type errorTranslatingReceiver struct {
	Receiver

	fromWire func(error) error
}

func (r *errorTranslatingReceiver) Receive(msg any) error {
	if err := r.Receiver.Receive(msg); err != nil {
		return r.fromWire(err)
	}
	return nil
}

func (r *errorTranslatingReceiver) Close() error {
	return r.fromWire(r.Receiver.Close())
}
