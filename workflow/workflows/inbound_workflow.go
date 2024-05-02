package workflows

import (
	"github.com/luongdev/fsflow/freeswitch"
	"github.com/luongdev/fsflow/shared"
	"github.com/luongdev/fsflow/workflow/activities"
	"go.uber.org/cadence/workflow"
	"go.uber.org/zap"
	"time"
)

type InboundWorkflowInput struct {
	ANI         string        `json:"ani"`
	DNIS        string        `json:"dnis"`
	Domain      string        `json:"domain"`
	SessionId   string        `json:"sessionId"`
	Initializer string        `json:"initializer"`
	Timeout     time.Duration `json:"timeout"`
}

const InitCompletedSignal = "init_completed"

type InboundWorkflow struct {
	fsClient *freeswitch.SocketClient
}

func (w *InboundWorkflow) Name() string {
	return "workflows.InboundWorkflow"
}

func NewInboundWorkflow(fsClient *freeswitch.SocketClient) *InboundWorkflow {
	return &InboundWorkflow{fsClient: fsClient}
}

func (w *InboundWorkflow) Handler() shared.WorkflowFunc {
	return func(ctx workflow.Context, i interface{}) (shared.WorkflowOutput, error) {
		logger := workflow.GetLogger(ctx)
		output := shared.WorkflowOutput{Success: false, Metadata: make(shared.Metadata)}
		input := InboundWorkflowInput{}
		ok := shared.Convert(i, &input)

		if !ok {
			logger.Error("Failed to cast input to InboundWorkflowInput")
			return output, shared.NewWorkflowInputError("Cannot cast input to InboundWorkflowInput")
		}

		ctx = workflow.WithActivityOptions(
			ctx,
			workflow.ActivityOptions{ScheduleToStartTimeout: time.Second, StartToCloseTimeout: input.Timeout},
		)

		siActivity := activities.NewSessionInitActivity(w.fsClient)
		f := workflow.ExecuteActivity(ctx, siActivity.Handler(), activities.SessionInitActivityInput{
			ANI:         input.ANI,
			DNIS:        input.DNIS,
			Domain:      input.Domain,
			Initializer: input.Initializer,
			Timeout:     input.Timeout,
		})

		if err := f.Get(ctx, &output); err != nil || !output.Success {
			logger.Error("Failed to execute SessionInitActivity", zap.Any("output", output), zap.Error(err))
			return output, err
		}

		switch output.Metadata[shared.Action].(string) {
		case string(shared.Bridge):
			break
		case string(shared.Hangup):
			hupActivity := activities.NewHangupActivity(w.fsClient)
			hi := activities.HangupActivityInput{SessionId: input.SessionId}
			if output.Metadata[shared.HangupCause] != nil {
				hi.HangupCause = output.Metadata[shared.HangupCause].(string)
			}
			err := workflow.ExecuteActivity(ctx, hupActivity.Handler(), hi).Get(ctx, &output)
			if err != nil {
				logger.Error("Failed to execute HangupActivity", zap.Any("output", output), zap.Error(err))
				return output, err
			}
			break
		case string(shared.Originate):
			if output.Metadata[shared.Destination] == nil {
				logger.Error("Missing required metadata", zap.Any("output", output))
				return output, shared.RequireField("destination")
			}

			if output.Metadata[shared.Gateway] == nil {
				logger.Error("Missing required metadata", zap.Any("output", output))
				return output, shared.RequireField("gateway")
			}

			oi := activities.OriginateActivityInput{
				Timeout:     input.Timeout,
				Destination: output.Metadata[shared.Destination].(string),
				Gateway:     output.Metadata[shared.Gateway].(string),
				AllowReject: true,
				AutoAnswer:  false,
				Direction:   freeswitch.Inbound,
			}
			if output.Metadata[shared.Profile] != nil {
				oi.Profile = output.Metadata[shared.Profile].(string)
			}

			origActivity := activities.NewOriginateActivity(w.fsClient)
			err := workflow.ExecuteActivity(ctx, origActivity.Handler(), oi).Get(ctx, &output)
			if err != nil || !output.Success {
				logger.Error("Failed to execute OriginateActivity", zap.Any("output", output), zap.Error(err))
				return output, err
			}

			if output.Metadata[shared.Uid] == nil {
				logger.Error("Missing required metadata", zap.Any("output", output))
				return output, shared.RequireField("uid")
			}

			brActivity := activities.NewBridgeActivity(w.fsClient)
			bi := activities.BridgeActivityInput{
				Originator: input.SessionId,
				Originatee: output.Metadata[shared.Uid].(string),
			}

			err = workflow.ExecuteActivity(ctx, brActivity.Handler(), bi).Get(ctx, &output)
			if err != nil || !output.Success {
				logger.Error("Failed to execute BridgeActivity", zap.Any("output", output), zap.Error(err))
				return output, err
			}

			break
		default:
			break
		}

		return output, nil
	}
}

var _ shared.FreeswitchWorkflow = (*InboundWorkflow)(nil)