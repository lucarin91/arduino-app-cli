#include <Arduino_RouterBridge.h>

void setup() {
  // setup the bridge for comunicating with the Linux subsystem.
  Bridge.begin();

  // start the monitor for debugging logs.
  Monitor.begin();

  // put your setup code here, to run once:
}

void loop() {
  // put your main code here, to run repeatedly:
}
