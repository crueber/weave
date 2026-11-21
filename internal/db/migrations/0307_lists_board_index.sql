-- Board statistics (boards.Stats, the project page tiles) read the columns of many boards at once by board_id.
CREATE INDEX IF NOT EXISTS lists_board ON lists(board_id, position);
