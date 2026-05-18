package ovr

import runtimeplan "ouvrier/internal/runtime"

func compileTrigger(source triggerSource) (runtimeplan.Trigger, error) {
	switch trigger := source.(type) {
	case httpTrigger:
		return runtimeplan.Trigger{
			Kind:   runtimeplan.TriggerHTTP,
			Method: trigger.method,
			Path:   trigger.path,
		}, nil
	case CronTrigger:
		return runtimeplan.Trigger{
			Kind: runtimeplan.TriggerCron,
			Expr: trigger.expr,
		}, nil
	case *CronTrigger:
		if trigger == nil {
			return runtimeplan.Trigger{}, ErrInvalidNode
		}
		return runtimeplan.Trigger{
			Kind: runtimeplan.TriggerCron,
			Expr: trigger.expr,
		}, nil
	case WebhookEndpoint:
		return runtimeplan.Trigger{
			Kind:  runtimeplan.TriggerWebhook,
			Value: trigger.value,
		}, nil
	case *WebhookEndpoint:
		if trigger == nil {
			return runtimeplan.Trigger{}, ErrInvalidNode
		}
		return runtimeplan.Trigger{
			Kind:  runtimeplan.TriggerWebhook,
			Value: trigger.value,
		}, nil
	case StreamTrigger:
		return runtimeplan.Trigger{
			Kind: runtimeplan.TriggerStream,
			URI:  trigger.uri,
		}, nil
	case *StreamTrigger:
		if trigger == nil {
			return runtimeplan.Trigger{}, ErrInvalidNode
		}
		return runtimeplan.Trigger{
			Kind: runtimeplan.TriggerStream,
			URI:  trigger.uri,
		}, nil
	default:
		return runtimeplan.Trigger{}, ErrInvalidNode
	}
}

func fromNodeFromNode(node Node) (fromNode, bool) {
	switch node := node.(type) {
	case fromNode:
		return node, true
	case *fromNode:
		if node == nil {
			return fromNode{}, false
		}
		return *node, true
	default:
		return fromNode{}, false
	}
}
