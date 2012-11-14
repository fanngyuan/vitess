import os
import shutil
from subprocess import check_call, Popen, CalledProcessError, PIPE
import sys
import time
import warnings
# Dropping a table inexplicably produces a warning despite
# the "IF EXISTS" clause. Squelch these warnings.
warnings.simplefilter("ignore")

import MySQLdb

import utils

vttop = os.environ['VTTOP']
vtroot = os.environ['VTROOT']

tablet_cell_map = {
    62344: 'nj',
    62044: 'nj',
    41983: 'nj',
    31981: 'ny',
}

class Tablet(object):
  default_uid = 62344
  default_port = 6700
  default_mysql_port = 3700
  seq = 0

  def __init__(self, tablet_uid=None, port=None, mysql_port=None, cell=None):
    self.tablet_uid = tablet_uid or (Tablet.default_uid + Tablet.seq)
    self.port = port or (Tablet.default_port + Tablet.seq)
    self.mysql_port = mysql_port or (Tablet.default_mysql_port + Tablet.seq)
    Tablet.seq += 1

    if cell:
      self.cell = cell
    else:
      self.cell = tablet_cell_map.get(tablet_uid, 'nj')
    self.proc = None

    # filled in during init_tablet
    self.keyspace = None
    self.shard = None
    self.zk_tablet_alias = None

    # utility variables
    self.zk_tablet_path = '/zk/test_%s/vt/tablets/%010d' % (self.cell, self.tablet_uid)
    self.zk_pid = self.zk_tablet_path + '/pid'

  def mysqlctl(self, cmd, quiet=False):
    utils.prog_compile(['mysqlctl'])

    logLevel = ''
    if utils.options.verbose and not quiet:
      logLevel = ' -log.level=INFO'

    return utils.run_bg(os.path.join(vtroot, 'bin', 'mysqlctl') +
                        logLevel + ' -tablet-uid %u ' % self.tablet_uid + cmd)

  def start_mysql(self):
    return self.mysqlctl('-port %u -mysql-port %u init' % (self.port, self.mysql_port), quiet=True)

  def teardown_mysql(self):
    return self.mysqlctl('-force teardown', quiet=True)

  def remove_tree(self):
    path = '/vt/vt_%010d' % self.tablet_uid
    try:
      shutil.rmtree(path)
    except OSError as e:
      if utils.options.verbose:
        print >> sys.stderr, e, path

  def connect(self, dbname=''):
    conn = MySQLdb.Connect(
        user='vt_dba',
        unix_socket='/vt/vt_%010d/mysql.sock' % self.tablet_uid,
        db=dbname)
    return conn, conn.cursor()

  # Query the MySQL instance directly
  def mquery(self, dbname, query, write=False):
    conn, cursor = self.connect(dbname)
    if write:
      conn.begin()

    cursor.execute(query)

    if write:
      conn.commit()

    try:
      return cursor.fetchall()
    finally:
      conn.close()

  def vquery(self, query, dbname='', user=None, password=None, driver=None,
                     verbose=False):
    utils.prog_compile(['vtclient2'])
    if (user is None) != (password is None):
      raise TypeError("you should provide either both or none of user and password")

    # for ZK paths to not have // in the path, that confuses things
    if dbname.startswith('/'):
      dbname = dbname[1:]
    server = "localhost:%u/%s" % (self.port, dbname)
    if user is not None:
      server = "%s:%s@%s" % (user, password, server)

    cmdline = [vtroot+'/bin/vtclient2', '-server', server]
    if driver:
      cmdline.extend(["-driver", driver])
    if verbose:
      cmdline.append("-verbose")
    cmdline.append('"%s"' % query)

    return utils.run(' '.join(cmdline), trap_output=True)


  def assert_table_count(self, dbname, table, n, where=''):
    result = self.mquery(dbname, 'select count(*) from ' + table + ' ' + where)
    if result[0][0] != n:
      raise utils.TestError("expected %u rows in %s" % (n, table), result)

  def populate(self, dbname, create_sql, insert_sqls=[]):
      self.create_db(dbname)
      self.mquery(dbname, create_sql)
      for q in insert_sqls:
        self.mquery(dbname, q, write=True)

  def create_db(self, name):
    self.mquery('', 'drop database if exists %s' % name)
    self.mquery('', 'create database %s' % name)

  def wait_check_db_var(self, name, value):
    for _ in range(3):
      try:
        return self.check_db_var(name, value)
      except utils.TestError as e:
        print >> sys.stderr, 'WARNING: ', e
      time.sleep(1.0)
    raise e

  def check_db_var(self, name, value):
    row = self.get_db_var(name)
    if row != (name, value):
      raise utils.TestError('variable not set correctly', name, row)

  def get_db_var(self, name):
    conn, cursor = self.connect()
    try:
      cursor.execute("show variables like '%s'" % name)
      return cursor.fetchone()
    finally:
      conn.close()

  def init_tablet(self, tablet_type, keyspace=None, shard=None, force=True, zk_parent_alias=None, key_start=None, key_end=None, start=False):
    self.keyspace = keyspace
    self.shard = shard
    if keyspace:
      self.zk_tablet_alias = "/zk/global/vt/keyspaces/%s/shards/%s/test_%s-%010d" % (self.keyspace, self.shard, self.cell, self.tablet_uid)
    else:
      self.zk_tablet_alias = ""

    if force:
      args = ['-force']
    else:
      args = []

    args.extend(['InitTablet',
                 'zk_tablet_path='+self.zk_tablet_path,
                 'hostname=localhost',
                 'mysql_port=%u' % self.mysql_port,
                 'port=%u' % self.port,
                 'tablet_type='+tablet_type])
    if keyspace:
      args.append('keyspace='+keyspace)
    if shard:
      args.append('shard_id='+shard)
    if zk_parent_alias:
      args.append('zk_parent_alias='+zk_parent_alias)
    if key_start:
      args.append('key_start='+key_start)
    if key_end:
      args.append('key_end='+key_end)

    utils.run_vtctl(' '.join(args))
    if start:
      self.start_vttablet()

  def start_vttablet(self, port=None, auth=False):
    """
    Starts a vttablet process, and returns it.
    The process is also saved in self.proc, so it's easy to kill as well.
    """
    utils.prog_compile(['vtaction',
                        'vttablet',
                        ])
    args = [os.path.join(vtroot, 'bin', 'vttablet'),
            '-port %s' % (port or self.port),
            '-tablet-path %s' % self.zk_tablet_path,
            '-logfile /vt/vt_%010d/vttablet.log' % self.tablet_uid]
    if auth:
      args.extend(['-auth-credentials', os.path.join(vttop, 'py', 'vttest', 'authcredentials_test.json')])

    self.proc = utils.run_bg(' '.join(args))
    utils.run(vtroot+'/bin/zk wait -e ' + self.zk_pid, stdout=utils.devnull)
    return self.proc

  def kill_vttablet(self):
    utils.kill_sub_process(self.proc)
