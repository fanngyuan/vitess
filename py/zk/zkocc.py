import hmac
import itertools
import logging

from net import bsonrpc
from net import gorpc

class ZkOccError(Exception):
  pass

#
# the ZkNode dict returned by these structures has the following members:
#
# Path     string
# Data     string
# Stat     ZkStat
# Children []string
# Cached   bool // the response comes from the zkocc cache
# Stale    bool // the response is stale because we're not connected
#
# ZkStat is a dict:
#
# czxid          long
# mzxid          long
# cTime          time.DateTime
# mTime          time.DateTime
# version        int
# cVersion       int
# aVersion       int
# ephemeralOwner long
# dataLength     int
# numChildren    int
# pzxid          long
#

# A simple, direct connection to a single zkocc server. Doesn't retry.
# You probably want to use ZkOccConnection instead.
class SimpleZkOccConnection(object):

  def __init__(self, addr, timeout, user=None, password=None):
    self.addr = addr
    self.timeout = timeout

    if bool(user) != bool(password):
      raise ValueError("You must provide either both or none of user and password.")
    self.user = user
    self.password = password
    self.use_auth = bool(user)

    self.client = bsonrpc.BsonRpcClient(self.uri, self.timeout)

  def dial(self):
    if self.client:
      self.client.close()
    if self.use_auth:
      self.authenticate()

  def authenticate(self):
    challenge = self.client.call('AuthenticatorCRAMMD5.GetNewChallenge', "").reply['Challenge']
    # CRAM-MD5 authentication.
    proof = self.user + " " + hmac.HMAC(self.password, challenge).hexdigest()
    self.client.call('AuthenticatorCRAMMD5.Authenticate', {"Proof": proof})

  @property
  def uri(self):
    if self.use_auth:
      return 'http://%s/_bson_rpc_/auth' % self.addr
    return 'http://%s/_bson_rpc_' % self.addr

  def close(self):
    self.client.close()

  __del__ = close

  # returns a ZkNode, see header
  def get(self, path):
    req = {'Path':path}
    try:
      return self.client.call('ZkReader.Get', req).reply
    except gorpc.GoRpcError as e:
      raise ZkOccError('get failed', e)

  # returns an array of ZkNode, see header
  def getv(self, paths):
    req = {'Paths':paths}
    try:
      return self.client.call('ZkReader.GetV', req).reply
    except gorpc.GoRpcError as e:
      raise ZkOccError('getv failed', e)

  # returns a ZkNode, see header
  def children(self, path):
    req = {'Path':path}
    try:
      return self.client.call('ZkReader.Children', req).reply
    except gorpc.GoRpcError as e:
      raise ZkOccError('children failed', e)

# A meta-connection that can connect to multiple alternate servers, and will
# retry a couple times. Calling dial before get/getv/children is optional,
# and will only do anything at all if authentication is enabled.
class ZkOccConnection(object):
  max_attempts = 2

  # addrs is a comma separated list of server:ip pairs.
  def __init__(self, addrs, local_cell, timeout, user=None, password=None):
    self.timeout = timeout
    addrs_array = addrs.split(',')
    self.addr_count = len(addrs_array)
    self.addrs = itertools.cycle(addrs_array)
    self.local_cell = local_cell

    if bool(user) != bool(password):
      raise ValueError("You must provide either both or none of user and password.")
    self.user = user
    self.password = password

    self.simpleConn = None

  def _resolve_path(self, zk_path):
    # Maps a 'meta-path' to a cell specific path.
    # '/zk/local/blah' -> '/zk/vb/blah'
    parts = zk_path.split('/')

    if len(parts) < 3:
      return zk_path

    if parts[2] != 'local':
      return zk_path

    parts[2] = self.local_cell
    return '/'.join(parts)

  def dial(self):
    if self.simpleConn:
      self.simpleConn.close()

    # try to connect to each server once (this will always work
    # if no auth is used, as then no connection is really established here)
    for i in xrange(self.addr_count):
      self.simpleConn = SimpleZkOccConnection(self.addrs.next(), self.timeout, self.user, self.password)
      try:
        self.simpleConn.dial()
        return
      except:
        pass

    self.simpleConn = None
    raise ZkOccError("Cannot dial to any server")

  def close(self):
    if self.simpleConn:
      self.simpleConn.close()
      self.simpleConn = None

  # returns a ZkNode, see header
  def get(self, path):
    if not self.simpleConn:
      self.dial()

    attempt = 0
    while True:
      try:
        return self.simpleConn.get(self._resolve_path(path))
      except Exception as e:
        attempt += 1
        if attempt >= self.max_attempts:
          logging.warning('zkocc: get command failed %u times: %s', attempt, e)
          raise ZkOccError('zkocc get command failed %u times: %s' % (attempt, e))

        # try the next server if there is one
        if self.addr_count > 1:
          self.dial()

  # returns an array of ZkNode, see header
  def getv(self, paths):
    if not self.simpleConn:
      self.dial()

    attempt = 0
    while True:
      try:
        return self.simpleConn.getv([self._resolve_path(p) for p in paths])
      except Exception as e:
        attempt += 1
        if attempt >= self.max_attempts:
          logging.warning('zkocc: getv command failed %u times: %s', attempt, e)
          raise ZkOccError('zkocc getv command failed %u times: %s' % (attempt, e))

        # try the next server if there is one
        if self.addr_count > 1:
          self.dial()

  # returns a ZkNode, see header
  def children(self, path):
    if not self.simpleConn:
      self.dial()

    attempt = 0
    while True:
      try:
        return self.simpleConn.children(self._resolve_path(path))
      except Exception as e:
        attempt += 1
        if attempt >= self.max_attempts:
          logging.warning('zkocc: children command failed %u times: %s', attempt, e)
          raise ZkOccError('zkocc children command failed %u times: %s' % (attempt, e))

        # try the next server if there is one
        if self.addr_count > 1:
          self.dial()
