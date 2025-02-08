#!/bin/bash
find testdata -maxdepth 1 -type d \( ! -name testdata \) | xargs -n 1 -I % bash -c "cd '%' && buf generate"
