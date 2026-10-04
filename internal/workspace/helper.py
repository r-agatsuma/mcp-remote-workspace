# Embedded in the daemon and executed only inside the selected container.
# Requests are JSON on stdin, never interpolated into code or a shell command.
import errno
import hashlib
import json
import os
import selectors
import signal
import stat
import subprocess
import sys
import time
import uuid


class OperationError(Exception):
    pass


def components(value, file=False):
    if not value or value.startswith('/') or '\x00' in value:
        raise OperationError('invalid_path')
    parts = value.split('/')
    if '..' in parts:
        raise OperationError('invalid_path')
    parts = [p for p in parts if p not in ('', '.')]
    if file and not parts:
        raise OperationError('invalid_path')
    return parts


def directory(parts):
    # Pin each directory before resolving the next component. O_NOFOLLOW on
    # every open rejects symlinks, including a replaced workspace root, without
    # a realpath/open time-of-check/time-of-use gap.
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
    fd = os.open('/workspace', flags)
    try:
        for part in parts:
            child = os.open(part, flags, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def text_result(parts, data):
    return {'path': '/'.join(parts), 'size_bytes': len(data),
            'sha256': hashlib.sha256(data).hexdigest()}


def write_text(req):
    parts = components(req['path'], file=True)
    data = req['content'].encode('utf-8', errors='strict')
    parent = directory(parts[:-1])
    temporary = None
    try:
        # Parents must already exist. Reject links and special-file targets.
        try:
            target = os.stat(parts[-1], dir_fd=parent, follow_symlinks=False)
            if not stat.S_ISREG(target.st_mode):
                raise OperationError('invalid_path')
        except FileNotFoundError:
            pass
        temporary = '.mcp-write-' + uuid.uuid4().hex
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL |
                     os.O_NOFOLLOW | os.O_CLOEXEC, 0o644, dir_fd=parent)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        # Rename replaces the directory entry itself; a link substituted after
        # the check is never followed. Readers see the old or complete new file.
        os.replace(temporary, parts[-1], src_dir_fd=parent, dst_dir_fd=parent)
        temporary = None
        return text_result(parts, data)
    finally:
        if temporary is not None:
            os.unlink(temporary, dir_fd=parent)
        os.close(parent)


def read_text(req):
    parts = components(req['path'], file=True)
    parent = directory(parts[:-1])
    try:
        fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK |
                     os.O_CLOEXEC, dir_fd=parent)
    finally:
        os.close(parent)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise OperationError('invalid_path')
        limit = req['max_text_file_bytes']
        if info.st_size > limit:
            raise OperationError('size_limit')
        data = stream.read(limit + 1)
    # Also check the bytes read: the file may have grown after fstat.
    if len(data) > limit:
        raise OperationError('size_limit')
    if b'\x00' in data:
        raise OperationError('non_utf8')
    content = data.decode('utf-8', errors='strict')
    result = text_result(parts, data)
    result['content'] = content
    return result


def kill_group(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def execute(req):
    cwd = directory(components(req['cwd']))
    try:
        os.fchdir(cwd)
    finally:
        os.close(cwd)
    env = os.environ.copy()
    env.update(req.get('env') or {})
    # shell=False is explicit; environment overrides apply only to this child.
    process = subprocess.Popen(req['argv'], shell=False, env=env,
                               stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    limit = req['max_stream_bytes']
    buffers = [bytearray(), bytearray()]
    truncated = [False, False]
    deadline = time.monotonic() + req['timeout_seconds']
    timed_out = False
    try:
        with selectors.DefaultSelector() as selector:
            for i, stream in enumerate((process.stdout, process.stderr)):
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, i)
            while selector.get_map() or process.poll() is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    timed_out = True
                    kill_group(process)
                    break
                for key, _ in selector.select(min(remaining, 0.05)):
                    data = os.read(key.fileobj.fileno(), 65536)
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    i = key.data
                    space = limit - len(buffers[i])
                    buffers[i].extend(data[:space])
                    truncated[i] |= len(data) > space
            if timed_out:
                # Drain only a bounded amount after SIGKILL. Escaped descendants
                # holding pipes cannot postpone the response indefinitely.
                for key in list(selector.get_map().values()):
                    for _ in range(16):
                        try:
                            data = os.read(key.fileobj.fileno(), 65536)
                        except BlockingIOError:
                            break
                        if not data:
                            break
                        i = key.data
                        space = limit - len(buffers[i])
                        buffers[i].extend(data[:space])
                        truncated[i] |= len(data) > space
    finally:
        kill_group(process)
        process.stdout.close()
        process.stderr.close()
        process.wait()
    # Drop an incomplete UTF-8 suffix at the byte boundary and replace invalid
    # internal bytes; cap the encoded tool string too (replacement can expand).
    def output(i):
        text = bytes(buffers[i]).decode('utf-8', errors='replace')
        encoded = text.encode('utf-8')
        if len(encoded) > limit:
            truncated[i] = True
            text = encoded[:limit].decode('utf-8', errors='ignore')
        return text
    stdout, stderr = output(0), output(1)
    return {'exit_code': None if timed_out else process.returncode,
            'stdout': stdout, 'stderr': stderr,
            'stdout_truncated': truncated[0], 'stderr_truncated': truncated[1],
            'timed_out': timed_out}


def main():
    try:
        req = json.load(sys.stdin)
        operation = {'exec': execute, 'write_text': write_text,
                     'read_text': read_text}[req['operation']]
        response = {'result': operation(req)}
    except OperationError as error:
        response = {'error': str(error)}
    except UnicodeError:
        response = {'error': 'non_utf8'}
    except OSError as error:
        code = 'invalid_path' if error.errno in (errno.ELOOP, errno.ENOTDIR) else 'backend_error'
        response = {'error': code}
    except Exception:
        response = {'error': 'backend_error'}
    json.dump(response, sys.stdout, ensure_ascii=True)


if __name__ == '__main__':
    main()
