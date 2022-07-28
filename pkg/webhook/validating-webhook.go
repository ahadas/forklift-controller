package webhook

import (
	"net/http"

	validating_webhooks "github.com/konveyor/forklift-controller/pkg/util/webhooks/validating-webhooks"
	"github.com/konveyor/forklift-controller/pkg/webhook/admitters"
)

func ServeProviderCreate(resp http.ResponseWriter, req *http.Request) {
	validating_webhooks.Serve(resp, req, &admitters.ProviderAdmitter{})
}
