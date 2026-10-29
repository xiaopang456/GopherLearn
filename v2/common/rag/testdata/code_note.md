# RabbitMQ

## autoAck 的风险

我可以把消息确认理解为消费者给 RabbitMQ 的“收货回执”。

```go
msgs, err := channel.Consume(
    queue,
    "",
    true, // autoAck
    false,
    false,
    false,
    nil,
)
```

`autoAck=true` 时，消费者处理过程中崩溃，消息也可能已经被确认并永久丢失。

## 手动确认

处理成功后调用 `Ack`；失败时根据错误类型选择 `Nack`、重新入队或进入死信队列。
