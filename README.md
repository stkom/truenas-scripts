# truenas-scripts

Script collection for my NAS system.

## fan-controller

Fan controller for Linux/TrueNAS SCALE. See [fan-controller/README.md](fan-controller/README.md).

## set_hdd_erc.sh
Sets the Error Recovery Control (aka SCTERC or TLER) read and write values on your system's hard drives.

Source: https://github.com/Spearfoot/FreeNAS-scripts/blob/master/set_hdd_erc.sh

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Script `/root/scripts/set_hdd_erc.sh`.
