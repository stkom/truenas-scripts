# truenas-scripts

Script collection for my NAS system.

## fan-controller

Fan controller for Linux/TrueNAS SCALE. See [fan-controller/README.md](fan-controller/README.md).

## unlock-nas

Runs on a client machine: unlocks the encrypted dataset `tank/data` through the TrueNAS API, mounts the share at
`/mnt/nas` (needs an fstab entry and sudo) and warms the directory cache in the background. If the dataset is already
unlocked, no passphrase is asked for. A wrong passphrase can be retried, and the mount is retried while the share
restarts.

Requires [uv](https://docs.astral.sh/uv/), which installs the
[TrueNAS API client](https://github.com/truenas/api_client) automatically on first run.

### Setup

Create an API key in the TrueNAS UI (user menu → My API Keys). On the first run the script asks for the key, verifies
it with the NAS and stores it in `~/.config/unlock-nas/api-key` (mode `600`). To replace it later:

```sh
./unlock-nas --set-api-key
```

Alternatively, provide the key in the `TRUENAS_API_KEY` environment variable.

### Usage

```sh
./unlock-nas                 # unlock, mount and warm the cache
./unlock-nas --no-mount      # only unlock
./unlock-nas --help          # host, dataset, mount point and other options
```

## set_hdd_erc.sh
Sets the Error Recovery Control (aka SCTERC or TLER) read and write values on your system's hard drives.

Source: https://github.com/Spearfoot/FreeNAS-scripts/blob/master/set_hdd_erc.sh

### Usage
In Tasks -> Init/Shutdown Scripts

Create a new entry as Post Init Script `/root/scripts/set_hdd_erc.sh`.
