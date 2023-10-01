# truenas-scripts

Script collection for my NAS system.

## spinpid2.sh
### Description
Fan Scripts for Supermicro Boards Using PID Logic by user [Glorious1](https://www.truenas.com/community/members/glorious1.45019/).

Source: https://www.truenas.com/community/resources/fan-scripts-for-supermicro-boards-using-pid-logic.24/

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Command `tmux new-session -d -s spinpid2 '/root/scripts/spinpid2.sh'`.

## set_hdd_erc.sh
### Description
Sets the Error Recovery Control (aka SCTERC or TLER) read and write values on your system's hard drives.

Source: https://github.com/Spearfoot/FreeNAS-scripts/blob/master/set_hdd_erc.sh

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Script `/root/scripts/set_hdd_erc.sh`.
