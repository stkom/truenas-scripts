# truenas-scripts

Script collection for my NAS system.

## hybrid_fan_controller.pl
Script to regulate fan speed.

Source: https://github.com/mrstux/hybrid_fan_control

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Command `tmux new-session -d -s fanscript '/root/scripts/hybrid_fan_controller.pl'`.

## set_hdd_erc.sh
Sets the Error Recovery Control (aka SCTERC or TLER) read and write values on your system's hard drives.

Source: https://github.com/Spearfoot/FreeNAS-scripts/blob/master/set_hdd_erc.sh

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Script `/root/scripts/set_hdd_erc.sh`.
