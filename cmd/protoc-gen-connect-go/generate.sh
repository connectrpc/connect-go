#!/bin/bash
find testdata -maxdepth 1 -type d \( ! -name testdata \) -exec bash -c "cd '{}' && buf generate" \;
