package safestop

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafestop(t *testing.T) {
	//Channels needed to pass
	completeChan := make(chan bool, 1)
	errorChan := make(chan bool, 1)
	firstHandlerChan := make(chan bool, 1)
	secondHandlerChan := make(chan bool, 1)

	//IDs for handlers
	idFirst := "first-handler"
	idSecond := "second-handler"

	//Create on complete and on error funcs
	onComplete := func(errors map[string]error) {
		//We are expecting errors to not be nil
		require.NotNil(t, errors)

		assert.Equal(t, 2, len(errors))
		assert.Nil(t, errors[idFirst])
		assert.NotNil(t, errors[idSecond])
		completeChan <- true
	}

	onError := func(id string, err error) {
		assert.Equal(t, idSecond, id)
		assert.NotNil(t, err)
		errorChan <- true
	}

	//Setup a safestop
	ss := NewSafeStop(&Opts{
		Signals:    nil,
		OnComplete: onComplete,
		OnError:    onError,
		Timeout:    time.Minute,
	})

	ss.RegisterHandler(idFirst, func(ctx context.Context) error {
		//Be successful, so just return nothing
		firstHandlerChan <- true
		return nil
	})

	ss.RegisterHandler(idSecond, func(ctx context.Context) error {
		//Fail so we can test error handling
		secondHandlerChan <- true
		return errors.New("Failed because we wanted it to")
	})

	//We will now send the signal as if a SIGINT was called
	ss.signalsChannel <- syscall.SIGINT

	//All of these channels should pass. If we time out, we fail
	<-firstHandlerChan
	<-secondHandlerChan
	<-errorChan
	<-completeChan
}
