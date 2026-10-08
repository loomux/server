### Fixed

- The conversation stream reports a turn that starts and finishes right
  after the client connects (LOOM-137). Its baseline was taken on the
  first poll tick, half a second after connect, so a quick turn looked
  "already finished" and its terminal `dispatch_update` (or a message's
  `message_added`) was never sent; it is now taken at connect.
